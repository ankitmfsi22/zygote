package trace

import (
	"encoding/hex"
	"fmt"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/vfs"
)

// RunRecorder orchestrates the agent's live filesystem (World) and its
// nondeterministic effects (Journal), allowing discrete Steps to be captured.
type RunRecorder struct {
	world    *vfs.World
	journal  *journal.Recorder
	steps    []Step
	meta     map[string]string
	stepBase int
	auto     bool
}

// Record begins a new trace tracking session on top of a store.
func Record(store vfs.Store, opts ...Option) *RunRecorder {
	r := &RunRecorder{
		world:   vfs.NewWorld(store),
		journal: journal.NewRecorder(),
		steps:   make([]Step, 0),
		meta:    make(map[string]string),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Set stores arbitrary string metadata for the run (e.g. model version, commit).
func (r *RunRecorder) Set(key, value string) {
	r.meta[key] = value
}

// World returns the live Virtual File System the agent is currently editing.
func (r *RunRecorder) World() *vfs.World {
	return r.world
}

// Recorder returns the journal for logging nondeterministic effects.
func (r *RunRecorder) Recorder() *journal.Recorder {
	return r.journal
}

// Step captures the current world state and effect count as a named boundary.
func (r *RunRecorder) Step(name string) {
	r.addStep(name, nil)
}

func (r *RunRecorder) addStep(name string, metadata map[string]string) *Step {
	snap := r.world.Snapshot()
	effs := len(r.journal.Entries())

	step := Step{
		N:        r.stepBase,
		Name:     name,
		Root:     hex.EncodeToString(snap.Root[:]),
		Effects:  effs,
		Metadata: metadata,
	}

	r.steps = append(r.steps, step)
	r.stepBase++
	return &step
}

// Recording returns the finalized manifest of all steps and effects.
func (r *RunRecorder) Recording() Recording {
	return Recording{
		Version:       1,
		HashAlgorithm: "blake3",
		Producer:      "zygote-cli",
		Meta:          r.meta,
		Steps:         r.steps,
		Effects:       r.journal.Entries(),
	}
}

// RunReplayer orchestrates the replay of a run, checking step boundaries against a Bundle.
type RunReplayer struct {
	bundle   *Bundle
	replayer *journal.Replayer
	world    *vfs.World
	stepIdx  int
	effects  int
	auto     bool
}

// Replay initializes a RunReplayer from a Bundle.
func Replay(store vfs.Store, bundle *Bundle) (*RunReplayer, error) {
	if len(bundle.Recording.Steps) == 0 {
		return nil, fmt.Errorf("bundle has no steps")
	}

	// Initialize the world at the first step
	rootHex := bundle.Recording.Steps[0].Root
	hBytes, err := hex.DecodeString(rootHex)
	if err != nil {
		return nil, fmt.Errorf("invalid root hash in bundle: %w", err)
	}

	var h vfs.Hash
	copy(h[:], hBytes)
	world := vfs.Fork(store, vfs.Snapshot{Root: h})

	resolver := journal.WithResolver(func(e journal.Entry) ([]byte, error) {
		return effectValue(store, e)
	})

	return &RunReplayer{
		bundle:   bundle,
		replayer: journal.NewReplayer(bundle.Recording.Effects, resolver),
		world:    world,
		stepIdx:  1, // We start checking from step 1 (step 0 is the init state)
		auto:     bundle.Recording.Meta[autoStepMetaKey] == "true",
	}, nil
}

// Replayer returns the underlying effect replayer.
func (r *RunReplayer) Replayer() *journal.Replayer {
	return r.replayer
}

// World returns the current active world being replayed.
func (r *RunReplayer) World() *vfs.World {
	return r.world
}

// Step verifies the current world against the next expected step in the bundle.
func (r *RunReplayer) Step(name string) (Check, error) {
	if r.stepIdx >= len(r.bundle.Recording.Steps) {
		return Check{}, &journal.DivergenceError{Class: journal.ClassExhausted, Message: "agent executed more steps than recorded", Step: r.stepIdx}
	}

	expectedStep := r.bundle.Recording.Steps[r.stepIdx]
	if expectedStep.Name != name {
		return Check{}, &journal.DivergenceError{Class: journal.ClassStepMismatch, Message: fmt.Sprintf("expected %q, got %q", expectedStep.Name, name), Step: r.stepIdx}
	}

	actualSnap := r.world.Snapshot()

	hBytes, _ := hex.DecodeString(expectedStep.Root)
	var expectedHash vfs.Hash
	copy(expectedHash[:], hBytes)

	chk := Check{
		N:        expectedStep.N,
		Name:     expectedStep.Name,
		Expected: vfs.Snapshot{Root: expectedHash},
		Actual:   actualSnap,
		Match:    expectedHash == actualSnap.Root,
	}

	r.stepIdx++
	if !chk.Match {
		return chk, &journal.DivergenceError{
			Class:   journal.ClassWorldMismatch,
			Message: fmt.Sprintf("world hash mismatch: expected %x, got %x", expectedHash[:], actualSnap.Root[:]),
			Step:    chk.N,
		}
	}
	return chk, nil
}
