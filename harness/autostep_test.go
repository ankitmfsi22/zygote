package harness_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

func effectLine(t *testing.T, id int, op, key, value string) string {
	t.Helper()
	params, err := json.Marshal(harness.EffectParams{Op: op, Key: key, Value: []byte(value)})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"effect","params":%s,"id":%d}`, params, id)
}

func stepLine(id int, name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"step","params":{"name":%q},"id":%d}`, name, id)
}

func serve(h harness.Handler, lines ...string) error {
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	return harness.NewServer(in, &bytes.Buffer{}, h).Serve()
}

func recordedNames(rec trace.Recording) []string {
	names := []string{}
	for _, s := range rec.Steps {
		names = append(names, s.Name)
	}
	return names
}

func newAutoRecorder() (*trace.RunRecorder, vfs.Store) {
	store := vfs.NewMemStore()
	r := trace.Record(store, trace.WithAutoStep())
	r.Step("init")
	return r, store
}

func stepLessAgent(t *testing.T, key string) []string {
	t.Helper()
	return []string{
		effectLine(t, 1, "tool:search", key, "hit"),
		effectLine(t, 2, "model", "prompt", "answer"),
	}
}

func TestAutoStepHarnessWithoutExplicitSteps(t *testing.T) {
	r, _ := newAutoRecorder()

	if err := serve(harness.NewRecordHandler(r), stepLessAgent(t, "q")...); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	rec := r.Recording()
	want := []string{"init", "auto:tool:search", "auto:final"}
	if !reflect.DeepEqual(recordedNames(rec), want) {
		t.Fatalf("unexpected steps %v", recordedNames(rec))
	}
	if rec.Steps[1].Effects != 1 || rec.Steps[2].Effects != 2 {
		t.Fatalf("unexpected effect counts %+v", rec.Steps)
	}
}

func TestAutoStepHarnessExplicitStepsStillWork(t *testing.T) {
	r, _ := newAutoRecorder()

	lines := []string{
		effectLine(t, 1, "tool:search", "q", "hit"),
		stepLine(2, "mine"),
		effectLine(t, 3, "model", "prompt", "answer"),
	}
	if err := serve(harness.NewRecordHandler(r), lines...); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	want := []string{"init", "auto:tool:search", "mine", "auto:final"}
	if got := recordedNames(r.Recording()); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected steps %v", got)
	}
}

func TestAutoStepHarnessReplayWithStepLessAgent(t *testing.T) {
	r, store := newAutoRecorder()
	if err := serve(harness.NewRecordHandler(r), stepLessAgent(t, "q")...); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(harness.NewReplayHandler(rr), stepLessAgent(t, "q")...); err != nil {
		t.Fatalf("replay diverged: %v", err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("replay flush diverged: %v", err)
	}
}

func TestAutoStepHarnessReplayChangedToolKey(t *testing.T) {
	r, store := newAutoRecorder()
	if err := serve(harness.NewRecordHandler(r), stepLessAgent(t, "q")...); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	err = serve(harness.NewReplayHandler(rr), stepLessAgent(t, "changed")...)

	var div *journal.DivergenceError
	if !errors.As(err, &div) || div.Class != journal.ClassEffectMismatch {
		t.Fatalf("expected effect-mismatch, got %v", err)
	}
}
