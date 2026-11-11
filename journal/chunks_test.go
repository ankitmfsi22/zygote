package journal_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/journal"
)

func TestValidateChunks(t *testing.T) {
	cases := []struct {
		name   string
		chunks []int
		total  int
		ok     bool
	}{
		{"none", nil, 5, true},
		{"empty", []int{}, 5, true},
		{"exact", []int{2, 3}, 5, true},
		{"single", []int{5}, 5, true},
		{"short", []int{2, 2}, 5, false},
		{"long", []int{3, 3}, 5, false},
		{"zero", []int{0, 5}, 5, false},
		{"negative", []int{-1, 6}, 5, false},
		{"overflow", []int{int(^uint(0) >> 1), 1}, 5, false},
	}
	for _, c := range cases {
		err := journal.ValidateChunks(c.chunks, c.total)
		if (err == nil) != c.ok {
			t.Errorf("%s: ok=%v, got %v", c.name, c.ok, err)
		}
	}
}

func TestRecordChunksKeepsBoundaries(t *testing.T) {
	r := journal.NewRecorder()
	chunks := []int{3, 2}

	val, got, err := r.RecordChunks("model", "p", func() ([]byte, []int, error) {
		return []byte("hello"), chunks, nil
	})
	if err != nil || string(val) != "hello" || !reflect.DeepEqual(got, chunks) {
		t.Fatalf("unexpected result %q %v %v", val, got, err)
	}

	chunks[0] = 99
	entries := r.Entries()
	if len(entries) != 1 || !reflect.DeepEqual(entries[0].Chunks, []int{3, 2}) {
		t.Fatalf("recorded chunks must not alias the caller's slice: %+v", entries)
	}
	entries[0].Chunks[0] = 77
	if !reflect.DeepEqual(r.Entries()[0].Chunks, []int{3, 2}) {
		t.Fatal("Entries must return a copy of the chunks")
	}
}

func TestRecordChunksInvalidIsNotLogged(t *testing.T) {
	for _, chunks := range [][]int{{1, 1}, {0, 5}, {-1, 6}, {9}} {
		r := journal.NewRecorder()
		_, _, err := r.RecordChunks("model", "p", func() ([]byte, []int, error) {
			return []byte("hello"), chunks, nil
		})
		if err == nil {
			t.Fatalf("chunks %v: expected error", chunks)
		}
		if len(r.Entries()) != 0 {
			t.Fatalf("chunks %v: invalid effect was logged", chunks)
		}
	}
}

func TestRecordChunksFailedEffectIsNotLogged(t *testing.T) {
	r := journal.NewRecorder()
	_, _, err := r.RecordChunks("model", "p", func() ([]byte, []int, error) {
		return nil, nil, errors.New("stream broke")
	})
	if err == nil || len(r.Entries()) != 0 {
		t.Fatalf("failed stream must not be logged: %v %v", err, r.Entries())
	}
}

func TestRecordWithoutChunksHasNoChunkMetadata(t *testing.T) {
	r := journal.NewRecorder()
	if _, err := r.Record("model", "p", func() ([]byte, error) { return []byte("v"), nil }); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r.Entries()[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"chunks", "value_ref", "value_size"} {
		if strings.Contains(string(b), field) {
			t.Fatalf("unexpected %s in %s", field, b)
		}
	}
}

func TestReplayChunksRoundTrip(t *testing.T) {
	r := journal.NewRecorder()
	if _, _, err := r.RecordChunks("model", "p", func() ([]byte, []int, error) {
		return []byte("abcdef"), []int{1, 2, 3}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Record("clock", "now", func() ([]byte, error) { return []byte("t"), nil }); err != nil {
		t.Fatal(err)
	}

	rp := journal.NewReplayer(r.Entries())
	val, chunks, err := rp.ReplayChunks("model", "p")
	if err != nil || string(val) != "abcdef" || !reflect.DeepEqual(chunks, []int{1, 2, 3}) {
		t.Fatalf("unexpected replay %q %v %v", val, chunks, err)
	}
	val, chunks, err = rp.ReplayChunks("clock", "now")
	if err != nil || string(val) != "t" || chunks != nil {
		t.Fatalf("unstreamed effect must replay without chunks: %q %v %v", val, chunks, err)
	}
}

func TestReplayPlainStillWorksOnStreamedEntry(t *testing.T) {
	rp := journal.NewReplayer([]journal.Entry{{Op: "model", Key: "p", Value: []byte("abc"), Chunks: []int{1, 2}}})
	val, err := rp.Replay("model", "p")
	if err != nil || string(val) != "abc" {
		t.Fatalf("unexpected replay %q %v", val, err)
	}
}

func TestModelStreamRoundTripAcrossRuneBoundary(t *testing.T) {
	euro := []byte("€")
	parts := [][]byte{[]byte("a"), euro[:2], {}, euro[2:], []byte("!")}

	r := journal.NewRecorder()
	got, err := journal.ModelStreamRecord(r, "p", func() ([][]byte, error) { return parts, nil })
	if err != nil || len(got) != len(parts) {
		t.Fatalf("unexpected record result %v %v", got, err)
	}

	entry := r.Entries()[0]
	if string(entry.Value) != "a€!" || !reflect.DeepEqual(entry.Chunks, []int{1, 2, 1, 1}) {
		t.Fatalf("unexpected entry %+v", entry)
	}

	replayed, err := journal.ModelStreamReplay(journal.NewReplayer(r.Entries()), "p")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("a"), euro[:2], euro[2:], []byte("!")}
	if !reflect.DeepEqual(replayed, want) {
		t.Fatalf("unexpected replay %q", replayed)
	}
}

func TestModelStreamReplayOfUnstreamedEntryIsOneChunk(t *testing.T) {
	rp := journal.NewReplayer([]journal.Entry{{Op: "model", Key: "p", Value: []byte("whole")}})
	parts, err := journal.ModelStreamReplay(rp, "p")
	if err != nil || len(parts) != 1 || string(parts[0]) != "whole" {
		t.Fatalf("unexpected parts %q %v", parts, err)
	}
}

func TestModelStreamReplayRejectsInconsistentChunks(t *testing.T) {
	rp := journal.NewReplayer([]journal.Entry{{Op: "model", Key: "p", Value: []byte("abc"), Chunks: []int{1, 1}}})
	if _, err := journal.ModelStreamReplay(rp, "p"); err == nil {
		t.Fatal("expected an error for chunks that do not match the value")
	}
}

func TestModelStreamRecordFailureIsNotLogged(t *testing.T) {
	r := journal.NewRecorder()
	if _, err := journal.ModelStreamRecord(r, "p", func() ([][]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("expected error")
	}
	if len(r.Entries()) != 0 {
		t.Fatal("failed stream was logged")
	}
}

func divergenceClass(t *testing.T, err error) journal.DivergenceClass {
	t.Helper()
	var div *journal.DivergenceError
	if !errors.As(err, &div) {
		t.Fatalf("expected a divergence, got %v", err)
	}
	return div.Class
}

func TestReplayRefWithoutResolverFailsClosed(t *testing.T) {
	rp := journal.NewReplayer([]journal.Entry{{Op: "model", Key: "p", ValueRef: strings.Repeat("a", 64)}})
	val, err := rp.Replay("model", "p")
	if err == nil || val != nil {
		t.Fatalf("expected failure, got %q %v", val, err)
	}
	if divergenceClass(t, err) != journal.ClassMissingObject {
		t.Fatalf("unexpected class: %v", err)
	}
}

func TestReplayResolverSuppliesValue(t *testing.T) {
	entry := journal.Entry{Op: "model", Key: "p", ValueRef: strings.Repeat("a", 64), Chunks: []int{2, 3}}
	var seen journal.Entry
	rp := journal.NewReplayer([]journal.Entry{entry}, journal.WithResolver(func(e journal.Entry) ([]byte, error) {
		seen = e
		return []byte("hello"), nil
	}))

	val, chunks, err := rp.ReplayChunks("model", "p")
	if err != nil || string(val) != "hello" || !reflect.DeepEqual(chunks, []int{2, 3}) {
		t.Fatalf("unexpected replay %q %v %v", val, chunks, err)
	}
	if seen.ValueRef != entry.ValueRef {
		t.Fatalf("resolver got %+v", seen)
	}
}

func TestReplayResolverFailureIsMissingObjectAndDoesNotAdvance(t *testing.T) {
	calls := 0
	rp := journal.NewReplayer(
		[]journal.Entry{{Op: "model", Key: "p", ValueRef: strings.Repeat("b", 64)}},
		journal.WithResolver(func(journal.Entry) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("object gone")
			}
			return []byte("back"), nil
		}),
	)
	_, err := rp.Replay("model", "p")
	if err == nil || divergenceClass(t, err) != journal.ClassMissingObject || !strings.Contains(err.Error(), "object gone") {
		t.Fatalf("unexpected error %v", err)
	}
	val, err := rp.Replay("model", "p")
	if err != nil || string(val) != "back" {
		t.Fatalf("failed resolution consumed the entry: %q %v", val, err)
	}
}

func TestReplayEntryWithValueAndRefFailsClosed(t *testing.T) {
	rp := journal.NewReplayer(
		[]journal.Entry{{Op: "model", Key: "p", Value: []byte("x"), ValueRef: strings.Repeat("c", 64)}},
		journal.WithResolver(func(journal.Entry) ([]byte, error) { return []byte("y"), nil }),
	)
	_, err := rp.Replay("model", "p")
	if err == nil || divergenceClass(t, err) != journal.ClassMissingObject {
		t.Fatalf("expected missing-object divergence, got %v", err)
	}
}

func TestEntryJSONOldFormatUnchanged(t *testing.T) {
	old := `{"seq":2,"op":"model","key":"k","value":"aGk="}`
	var e journal.Entry
	if err := json.Unmarshal([]byte(old), &e); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.Value, []byte("hi")) || e.ValueRef != "" || e.ValueSize != 0 || e.Chunks != nil {
		t.Fatalf("unexpected entry %+v", e)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != old {
		t.Fatalf("re-marshal changed the old format: %s", b)
	}
}

func TestEntryJSONNewFields(t *testing.T) {
	e := journal.Entry{Seq: 1, Op: "model", Key: "k", ValueRef: strings.Repeat("d", 64), ValueSize: 10, Chunks: []int{4, 6}}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"value"`) {
		t.Fatalf("value must be omitted when value_ref is set: %s", b)
	}
	var back journal.Entry
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, e) {
		t.Fatalf("round trip failed: %+v %v", back, err)
	}
}
