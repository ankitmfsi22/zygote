package trace_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

func value(v string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(v), nil }
}

func stepNames(rec trace.Recording) []string {
	names := []string{}
	for _, s := range rec.Steps {
		names = append(names, s.Name)
	}
	return names
}

func newAuto(t *testing.T) (*trace.RunRecorder, vfs.Store) {
	t.Helper()
	store := vfs.NewMemStore()
	r := trace.Record(store, trace.WithAutoStep())
	r.Step("init")
	return r, store
}

func TestAutoStepToolEffectClosesStep(t *testing.T) {
	r, _ := newAuto(t)

	val, step, err := r.RecordEffect("tool:search", "q", value("hit"))
	if err != nil || string(val) != "hit" {
		t.Fatalf("unexpected result %q %v", val, err)
	}
	if step == nil || step.Name != "auto:tool:search" || step.N != 1 || step.Effects != 1 {
		t.Fatalf("unexpected auto step %+v", step)
	}
	want := map[string]string{"auto": "true", "trigger": "tool:search", "seq": "0"}
	if !reflect.DeepEqual(step.Metadata, want) {
		t.Fatalf("unexpected metadata %v", step.Metadata)
	}

	rec := r.Recording()
	if !reflect.DeepEqual(stepNames(rec), []string{"init", "auto:tool:search"}) {
		t.Fatalf("unexpected steps %v", stepNames(rec))
	}
	if rec.Meta["auto_step"] != "true" {
		t.Fatalf("auto_step meta missing: %v", rec.Meta)
	}
}

func TestAutoStepNonToolEffectDoesNotClose(t *testing.T) {
	r, _ := newAuto(t)

	for _, op := range []string{"model", "clock", "http", "rand"} {
		_, step, err := r.RecordEffect(op, "k", value("v"))
		if err != nil || step != nil {
			t.Fatalf("op %s: unexpected step %+v err %v", op, step, err)
		}
	}
	if got := len(r.Recording().Steps); got != 1 {
		t.Fatalf("expected only init, got %d steps", got)
	}
}

func TestAutoStepFailedEffectDoesNotClose(t *testing.T) {
	r, _ := newAuto(t)

	_, step, err := r.RecordEffect("tool:x", "k", func() ([]byte, error) { return nil, errors.New("boom") })
	if err == nil || step != nil {
		t.Fatalf("expected error and no step, got %+v %v", step, err)
	}
	if len(r.Recording().Steps) != 1 || len(r.Recording().Effects) != 0 {
		t.Fatalf("failed effect must leave no trace: %+v", r.Recording())
	}
}

func TestAutoStepNoEmptySteps(t *testing.T) {
	r, _ := newAuto(t)

	if step := r.Finish(); step != nil {
		t.Fatalf("Finish with nothing outstanding produced %+v", step)
	}
	if _, _, err := r.RecordEffect("tool:a", "k", value("v")); err != nil {
		t.Fatal(err)
	}
	if step := r.Finish(); step != nil {
		t.Fatalf("Finish right after an auto step produced %+v", step)
	}
	if !reflect.DeepEqual(stepNames(r.Recording()), []string{"init", "auto:tool:a"}) {
		t.Fatalf("unexpected steps %v", stepNames(r.Recording()))
	}
}

func TestAutoStepFinishFlushesTrailingEffects(t *testing.T) {
	r, _ := newAuto(t)

	if _, _, err := r.RecordEffect("model", "p", value("v")); err != nil {
		t.Fatal(err)
	}
	step := r.Finish()
	if step == nil || step.Name != "auto:final" || step.Effects != 1 {
		t.Fatalf("unexpected final step %+v", step)
	}
	if !reflect.DeepEqual(step.Metadata, map[string]string{"auto": "true", "trigger": "final"}) {
		t.Fatalf("unexpected metadata %v", step.Metadata)
	}
	if again := r.Finish(); again != nil {
		t.Fatalf("second Finish produced %+v", again)
	}
	if !reflect.DeepEqual(stepNames(r.Recording()), []string{"init", "auto:final"}) {
		t.Fatalf("unexpected steps %v", stepNames(r.Recording()))
	}
}

func TestAutoStepFinishFlushesWorldChange(t *testing.T) {
	r, _ := newAuto(t)

	if err := r.World().WriteFile("a.txt", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := r.Finish()
	if step == nil || step.Name != "auto:final" || step.Effects != 0 {
		t.Fatalf("unexpected final step %+v", step)
	}
	if r.Finish() != nil {
		t.Fatal("Finish is not idempotent")
	}
	steps := r.Recording().Steps
	if len(steps) != 2 || steps[1].Root == steps[0].Root {
		t.Fatalf("final step must capture the changed world: %+v", steps)
	}
}

func TestAutoStepInterleavedWithExplicit(t *testing.T) {
	r, _ := newAuto(t)

	r.Step("plan")
	if _, _, err := r.RecordEffect("tool:a", "k1", value("v")); err != nil {
		t.Fatal(err)
	}
	r.Step("manual")
	if _, _, err := r.RecordEffect("tool:b", "k2", value("v")); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	rec := r.Recording()
	want := []string{"init", "plan", "auto:tool:a", "manual", "auto:tool:b"}
	if !reflect.DeepEqual(stepNames(rec), want) {
		t.Fatalf("unexpected steps %v", stepNames(rec))
	}
	for i, s := range rec.Steps {
		if s.N != i {
			t.Fatalf("step %d has N=%d", i, s.N)
		}
	}
	if rec.Steps[1].Metadata != nil || rec.Steps[3].Metadata != nil {
		t.Fatal("explicit steps must not carry auto metadata")
	}
}

func TestAutoStepOffByDefault(t *testing.T) {
	run := func(viaRecordEffect bool) trace.Recording {
		r := trace.Record(vfs.NewMemStore())
		r.Step("init")
		for _, op := range []string{"tool:a", "model", "tool:b"} {
			if viaRecordEffect {
				if _, step, err := r.RecordEffect(op, "k", value("v")); err != nil || step != nil {
					t.Fatalf("unexpected auto step %+v %v", step, err)
				}
			} else if _, err := r.Recorder().Record(op, "k", value("v")); err != nil {
				t.Fatal(err)
			}
		}
		if r.Finish() != nil {
			t.Fatal("Finish must be a no-op when auto-step is off")
		}
		r.Step("end")
		return r.Recording()
	}

	a, err := json.Marshal(run(true))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(run(false))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("recordings differ:\n%s\n%s", a, b)
	}
	if run(true).Meta["auto_step"] != "" {
		t.Fatal("auto_step meta must not be set by default")
	}
}

func replayOf(t *testing.T, r *trace.RunRecorder, store vfs.Store) *trace.RunReplayer {
	t.Helper()
	bundle := &trace.Bundle{Recording: r.Recording(), Store: store}
	rr, err := trace.Replay(store, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func recordAutoRun(t *testing.T) (*trace.RunRecorder, vfs.Store) {
	t.Helper()
	r, store := newAuto(t)
	if _, _, err := r.RecordEffect("tool:search", "q", value("hit")); err != nil {
		t.Fatal(err)
	}
	if err := r.World().WriteFile("out.txt", []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r.Finish() == nil {
		t.Fatal("expected a final step")
	}
	return r, store
}

func TestAutoStepReplayMatches(t *testing.T) {
	r, store := recordAutoRun(t)
	rr := replayOf(t, r, store)

	val, err := rr.ReplayEffect("tool:search", "q")
	if err != nil || string(val) != "hit" {
		t.Fatalf("unexpected replay %q %v", val, err)
	}
	if err := rr.World().WriteFile("out.txt", []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("replay diverged: %v", err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("second Finish diverged: %v", err)
	}
}

func asDivergence(t *testing.T, err error, class journal.DivergenceClass) {
	t.Helper()
	var div *journal.DivergenceError
	if !errors.As(err, &div) || div.Class != class {
		t.Fatalf("expected %s divergence, got %v", class, err)
	}
}

func TestAutoStepReplayEffectMismatch(t *testing.T) {
	r, store := recordAutoRun(t)
	rr := replayOf(t, r, store)

	_, err := rr.ReplayEffect("tool:search", "other")
	asDivergence(t, err, journal.ClassEffectMismatch)
}

func TestAutoStepReplayWorldMismatchAtAutoStep(t *testing.T) {
	r, store := newAuto(t)
	if err := r.World().WriteFile("a.txt", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RecordEffect("tool:search", "q", value("hit")); err != nil {
		t.Fatal(err)
	}
	rr := replayOf(t, r, store)

	_, err := rr.ReplayEffect("tool:search", "q")
	asDivergence(t, err, journal.ClassWorldMismatch)
}

func TestAutoStepReplayFinalMismatch(t *testing.T) {
	r, store := recordAutoRun(t)

	rr := replayOf(t, r, store)
	if _, err := rr.ReplayEffect("tool:search", "q"); err != nil {
		t.Fatal(err)
	}
	asDivergence(t, rr.Finish(), journal.ClassStepMismatch)

	rr = replayOf(t, r, store)
	if _, err := rr.ReplayEffect("tool:search", "q"); err != nil {
		t.Fatal(err)
	}
	if err := rr.World().WriteFile("out.txt", []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	asDivergence(t, rr.Finish(), journal.ClassWorldMismatch)
}

func TestAutoStepReplayWithoutMetaIsUnchanged(t *testing.T) {
	store := vfs.NewMemStore()
	r := trace.Record(store)
	r.Step("init")
	if _, err := r.Recorder().Record("tool:search", "q", value("hit")); err != nil {
		t.Fatal(err)
	}
	rr := replayOf(t, r, store)

	if _, err := rr.ReplayEffect("tool:search", "q"); err != nil {
		t.Fatalf("unexpected divergence: %v", err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("Finish must be a no-op without auto_step meta: %v", err)
	}
}
