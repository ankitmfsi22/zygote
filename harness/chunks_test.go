package harness_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/trace"
)

func chunkedLine(t *testing.T, id int, op, key, value string, chunks []int) string {
	t.Helper()
	params, err := json.Marshal(harness.EffectParams{Op: op, Key: key, Value: []byte(value), Chunks: chunks})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"effect","params":%s,"id":%d}`, params, id)
}

type rpcReply struct {
	Result *harness.EffectResult `json:"result"`
	Error  *harness.Error        `json:"error"`
}

func serveReplies(t *testing.T, h harness.Handler, lines ...string) ([]rpcReply, error) {
	t.Helper()
	var out bytes.Buffer
	err := harness.NewServer(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, h).Serve()
	var replies []rpcReply
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var r rpcReply
		if e := json.Unmarshal([]byte(l), &r); e != nil {
			t.Fatalf("bad reply %q: %v", l, e)
		}
		replies = append(replies, r)
	}
	return replies, err
}

func TestHarnessChunksRoundTrip(t *testing.T) {
	r, store := newAutoRecorder()
	lines := []string{
		chunkedLine(t, 1, "model", "p", "hello world", []int{5, 1, 5}),
		chunkedLine(t, 2, "http", "plain", "whole", nil),
	}

	replies, err := serveReplies(t, harness.NewRecordHandler(r), lines...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replies[0].Result.Chunks, []int{5, 1, 5}) || string(replies[0].Result.Value) != "hello world" {
		t.Fatalf("record reply %+v", replies[0].Result)
	}
	if replies[1].Result.Chunks != nil {
		t.Fatalf("unstreamed effect must not report chunks: %+v", replies[1].Result)
	}
	r.Finish()
	if got := r.Recording().Effects[0].Chunks; !reflect.DeepEqual(got, []int{5, 1, 5}) {
		t.Fatalf("recorded chunks %v", got)
	}

	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	replayLines := []string{
		effectLine(t, 1, "model", "p", ""),
		effectLine(t, 2, "http", "plain", ""),
	}
	replies, err = serveReplies(t, harness.NewReplayHandler(rr), replayLines...)
	if err != nil {
		t.Fatal(err)
	}
	if string(replies[0].Result.Value) != "hello world" || !reflect.DeepEqual(replies[0].Result.Chunks, []int{5, 1, 5}) {
		t.Fatalf("replay must return value and chunks: %+v", replies[0].Result)
	}
	if string(replies[1].Result.Value) != "whole" || replies[1].Result.Chunks != nil {
		t.Fatalf("replay of an unstreamed effect: %+v", replies[1].Result)
	}
	if err := rr.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessReplayIgnoresAgentSuppliedChunks(t *testing.T) {
	r, store := newAutoRecorder()
	if _, err := serveReplies(t, harness.NewRecordHandler(r), chunkedLine(t, 1, "model", "p", "abcd", []int{2, 2})); err != nil {
		t.Fatal(err)
	}
	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	replies, err := serveReplies(t, harness.NewReplayHandler(rr), chunkedLine(t, 1, "model", "p", "zzzz", []int{1, 3}))
	if err != nil {
		t.Fatal(err)
	}
	if string(replies[0].Result.Value) != "abcd" || !reflect.DeepEqual(replies[0].Result.Chunks, []int{2, 2}) {
		t.Fatalf("replay must answer from the recording: %+v", replies[0].Result)
	}
}

func TestHarnessInvalidChunksFailClosed(t *testing.T) {
	for name, chunks := range map[string][]int{"short": {1, 1}, "zero": {0, 4}, "negative": {-1, 5}, "long": {9}} {
		r, _ := newAutoRecorder()
		_, err := serveReplies(t, harness.NewRecordHandler(r), chunkedLine(t, 1, "model", "p", "abcd", chunks))
		if err == nil {
			t.Fatalf("%s: expected the run to terminate", name)
		}
		if len(r.Recording().Effects) != 0 {
			t.Fatalf("%s: invalid effect was recorded", name)
		}
	}
}

func TestHarnessEmptyChunksMeanNone(t *testing.T) {
	r, _ := newAutoRecorder()
	line := `{"jsonrpc":"2.0","method":"effect","params":{"op":"model","key":"p","value":"YWJj","chunks":[]},"id":1}`
	if _, err := serveReplies(t, harness.NewRecordHandler(r), line); err != nil {
		t.Fatal(err)
	}
	if got := r.Recording().Effects[0].Chunks; got != nil {
		t.Fatalf("unexpected chunks %v", got)
	}
}

type plainHandler struct {
	calls int
}

func (h *plainHandler) Handshake(int) (int, error) { return 1, nil }

func (h *plainHandler) Effect(_, _ string, value []byte) ([]byte, error) {
	h.calls++
	return value, nil
}

func (h *plainHandler) Step(string) (*harness.StepResult, error) {
	return &harness.StepResult{Match: true}, nil
}

func TestHarnessHandlerWithoutChunkSupportRejectsChunks(t *testing.T) {
	h := &plainHandler{}
	replies, err := serveReplies(t, h,
		chunkedLine(t, 1, "model", "p", "abcd", []int{2, 2}),
		effectLine(t, 2, "model", "p", "abcd"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if replies[0].Error == nil || replies[0].Error.Code != harness.ErrInvalidParams {
		t.Fatalf("expected ErrInvalidParams, got %+v", replies[0])
	}
	if replies[1].Result == nil || string(replies[1].Result.Value) != "abcd" || h.calls != 1 {
		t.Fatalf("plain effect must still work: %+v calls=%d", replies[1], h.calls)
	}
}

func TestHarnessStreamedToolEffectAutoSteps(t *testing.T) {
	r, store := newAutoRecorder()
	lines := []string{
		chunkedLine(t, 1, "tool:search", "q", "result text", []int{6, 5}),
		chunkedLine(t, 2, "model", "p", "answer", []int{3, 3}),
	}
	if _, err := serveReplies(t, harness.NewRecordHandler(r), lines...); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	want := []string{"init", "auto:tool:search", "auto:final"}
	if got := recordedNames(r.Recording()); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected steps %v", got)
	}

	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	replies, err := serveReplies(t, harness.NewReplayHandler(rr),
		effectLine(t, 1, "tool:search", "q", ""),
		effectLine(t, 2, "model", "p", ""),
	)
	if err != nil {
		t.Fatalf("replay diverged: %v", err)
	}
	if !reflect.DeepEqual(replies[0].Result.Chunks, []int{6, 5}) || !reflect.DeepEqual(replies[1].Result.Chunks, []int{3, 3}) {
		t.Fatalf("unexpected chunks %+v %+v", replies[0].Result, replies[1].Result)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("replay flush diverged: %v", err)
	}
}
