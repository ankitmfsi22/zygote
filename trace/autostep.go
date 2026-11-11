package trace

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/mindfire/zygote/journal"
)

const (
	autoStepMetaKey = "auto_step"
	autoStepPrefix  = "auto:"
	autoFinalName   = "auto:final"
	toolOpPrefix    = "tool:"
)

// Option configures a RunRecorder.
type Option func(*RunRecorder)

// WithAutoStep derives step boundaries from tool effects and from Finish.
func WithAutoStep() Option {
	return func(r *RunRecorder) {
		r.auto = true
		r.meta[autoStepMetaKey] = "true"
	}
}

func isAutoTrigger(op string) bool {
	return strings.HasPrefix(op, toolOpPrefix)
}

func autoStepName(op string) string {
	return autoStepPrefix + op
}

func autoToolMetadata(op string, seq int) map[string]string {
	return map[string]string{"auto": "true", "trigger": op, "seq": strconv.Itoa(seq)}
}

func autoFinalMetadata() map[string]string {
	return map[string]string{"auto": "true", "trigger": "final"}
}

func needsFinal(hasLast bool, lastEffects int, lastRoot string, effects int, root string) bool {
	if effects > lastEffects {
		return true
	}
	return hasLast && root != lastRoot
}

// RecordEffect records an effect and, in auto-step mode, closes a step after a tool effect.
func (r *RunRecorder) RecordEffect(op, key string, fn func() ([]byte, error)) ([]byte, *Step, error) {
	return r.RecordEffectChunks(op, key, func() ([]byte, []int, error) {
		v, err := fn()
		return v, nil, err
	})
}

// RecordEffectChunks records a streamed effect with its chunk sizes and, in auto-step mode, closes a step after a tool effect.
func (r *RunRecorder) RecordEffectChunks(op, key string, fn func() ([]byte, []int, error)) ([]byte, *Step, error) {
	val, _, err := r.journal.RecordChunks(op, key, fn)
	if err != nil {
		return nil, nil, err
	}
	if !r.auto || !isAutoTrigger(op) {
		return val, nil, nil
	}
	seq := len(r.journal.Entries()) - 1
	return val, r.addStep(autoStepName(op), autoToolMetadata(op, seq)), nil
}

// Finish closes a final auto step for trailing effects or world changes; it is idempotent.
func (r *RunRecorder) Finish() *Step {
	if !r.auto {
		return nil
	}
	hasLast := len(r.steps) > 0
	lastEffects, lastRoot := 0, ""
	if hasLast {
		last := r.steps[len(r.steps)-1]
		lastEffects, lastRoot = last.Effects, last.Root
	}
	snap := r.world.Snapshot()
	if !needsFinal(hasLast, lastEffects, lastRoot, len(r.journal.Entries()), hex.EncodeToString(snap.Root[:])) {
		return nil
	}
	return r.addStep(autoFinalName, autoFinalMetadata())
}

// ReplayEffect replays an effect and, in auto-step mode, verifies the step a tool effect closed.
func (r *RunReplayer) ReplayEffect(op, key string) ([]byte, error) {
	val, _, err := r.ReplayEffectChunks(op, key)
	return val, err
}

// ReplayEffectChunks replays an effect with its recorded chunk sizes and, in auto-step mode, verifies the step a tool effect closed.
func (r *RunReplayer) ReplayEffectChunks(op, key string) ([]byte, []int, error) {
	val, chunks, err := r.replayer.ReplayChunks(op, key)
	if err != nil {
		return nil, nil, err
	}
	r.effects++
	if !r.auto || !isAutoTrigger(op) {
		return val, chunks, nil
	}
	if _, err := r.Step(autoStepName(op)); err != nil {
		return nil, nil, err
	}
	return val, chunks, nil
}

// Finish verifies the final auto step the recording closed, if any.
func (r *RunReplayer) Finish() error {
	if !r.auto {
		return nil
	}
	steps := r.bundle.Recording.Steps
	last := steps[r.stepIdx-1]
	snap := r.world.Snapshot()
	if needsFinal(true, last.Effects, last.Root, r.effects, hex.EncodeToString(snap.Root[:])) {
		_, err := r.Step(autoFinalName)
		return err
	}
	if r.stepIdx < len(steps) && steps[r.stepIdx].Name == autoFinalName {
		return &journal.DivergenceError{
			Class:   journal.ClassStepMismatch,
			Message: fmt.Sprintf("expected %q, got no step", autoFinalName),
			Step:    r.stepIdx,
		}
	}
	return nil
}
