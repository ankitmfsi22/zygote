package trace_test

import (
	"errors"
	"testing"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

func wantDivergence(t *testing.T, err error, class journal.DivergenceClass, step int) {
	t.Helper()
	var div *journal.DivergenceError
	if !errors.As(err, &div) || div.Class != class || div.Step != step {
		t.Fatalf("want %s divergence at step %d, got %v", class, step, err)
	}
}

func mustEffect(t *testing.T, r *trace.RunRecorder, op, key string) {
	t.Helper()
	if _, _, err := r.RecordEffect(op, key, value("v")); err != nil {
		t.Fatal(err)
	}
}

func mustReplay(t *testing.T, rr *trace.RunReplayer, op, key string) {
	t.Helper()
	if _, err := rr.ReplayEffect(op, key); err != nil {
		t.Fatalf("replay %s/%s: %v", op, key, err)
	}
}

// recordExplicit records: init, effect, step "a", effect, step "b".
func recordExplicit(t *testing.T) (*trace.RunRecorder, vfs.Store) {
	t.Helper()
	store := vfs.NewMemStore()
	r := trace.Record(store)
	r.Step("init")
	mustEffect(t, r, "model", "k1")
	r.Step("a")
	mustEffect(t, r, "model", "k2")
	r.Step("b")
	return r, store
}

func TestReplayEffectDivergenceReportsStepOfFollowingSnapshot(t *testing.T) {
	r, store := recordExplicit(t)

	t.Run("before the first step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		_, err := rr.ReplayEffect("model", "other")
		wantDivergence(t, err, journal.ClassEffectMismatch, 1)
	})
	t.Run("between step 1 and step 2", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "k1")
		if _, err := rr.Step("a"); err != nil {
			t.Fatal(err)
		}
		_, err := rr.ReplayEffect("model", "other")
		wantDivergence(t, err, journal.ClassEffectMismatch, 2)
	})
	t.Run("exhausted before the first step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "k1")
		_, err := rr.ReplayEffect("model", "k2")
		if err != nil {
			t.Fatal(err)
		}
		_, err = rr.ReplayEffect("model", "k3")
		wantDivergence(t, err, journal.ClassExhausted, 1)
	})
	t.Run("exhausted after the last step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "k1")
		if _, err := rr.Step("a"); err != nil {
			t.Fatal(err)
		}
		mustReplay(t, rr, "model", "k2")
		if _, err := rr.Step("b"); err != nil {
			t.Fatal(err)
		}
		_, err := rr.ReplayEffect("model", "k3")
		wantDivergence(t, err, journal.ClassExhausted, 3)
	})
}

func TestAutoStepReplayEffectDivergenceReportsStep(t *testing.T) {
	store := vfs.NewMemStore()
	r := trace.Record(store, trace.WithAutoStep())
	r.Step("init")
	mustEffect(t, r, "model", "prompt")
	mustEffect(t, r, "tool:search", "q")
	mustEffect(t, r, "model", "prompt2")
	mustEffect(t, r, "tool:read", "f")

	t.Run("before the first auto step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		_, err := rr.ReplayEffect("model", "other")
		wantDivergence(t, err, journal.ClassEffectMismatch, 1)
	})
	t.Run("the tool effect that closes a step belongs to it", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "prompt")
		_, err := rr.ReplayEffect("tool:other", "q")
		wantDivergence(t, err, journal.ClassEffectMismatch, 1)
	})
	t.Run("after the first auto step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "prompt")
		mustReplay(t, rr, "tool:search", "q")
		_, err := rr.ReplayEffect("model", "other")
		wantDivergence(t, err, journal.ClassEffectMismatch, 2)
	})
	t.Run("exhausted after the last step", func(t *testing.T) {
		rr := replayOf(t, r, store)
		mustReplay(t, rr, "model", "prompt")
		mustReplay(t, rr, "tool:search", "q")
		mustReplay(t, rr, "model", "prompt2")
		mustReplay(t, rr, "tool:read", "f")
		_, err := rr.ReplayEffect("model", "more")
		wantDivergence(t, err, journal.ClassExhausted, 3)
	})
}
