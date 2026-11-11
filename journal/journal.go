// Package journal provides effects.
package journal

import (
	"fmt"
	"sync"
)

// Recorder maintains an append-only log of non-deterministic effects.
type Recorder struct {
	mu      sync.Mutex
	entries []Entry
	seq     int
}

// NewRecorder initializes a new empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		entries: make([]Entry, 0),
		seq:     0,
	}
}

// Record invokes the provided function and, if successful, logs the result.
// Fulfills FR-2.5: A failed effect aborts the recording and is not logged.
func (r *Recorder) Record(op, key string, fn func() ([]byte, error)) ([]byte, error) {
	val, _, err := r.RecordChunks(op, key, func() ([]byte, []int, error) {
		v, err := fn()
		return v, nil, err
	})
	return val, err
}

// RecordChunks records an effect whose value was streamed, keeping the chunk sizes as metadata (FR-2.8).
func (r *Recorder) RecordChunks(op, key string, fn func() ([]byte, []int, error)) ([]byte, []int, error) {
	val, chunks, err := fn()
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateChunks(chunks, len(val)); err != nil {
		return nil, nil, fmt.Errorf("invalid chunks for %s %q: %w", op, key, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := Entry{
		Seq:    r.seq,
		Op:     op,
		Key:    key,
		Value:  val,
		Chunks: cloneChunks(chunks),
	}
	r.entries = append(r.entries, entry)
	r.seq++

	return val, chunks, nil
}

func cloneChunks(chunks []int) []int {
	if len(chunks) == 0 {
		return nil
	}
	return append([]int(nil), chunks...)
}

// Entries returns a copy of the recorded effect log.
func (r *Recorder) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	for i := range out {
		out[i].Chunks = cloneChunks(out[i].Chunks)
	}
	return out
}

// Resolver loads the value an entry references by value_ref.
type Resolver func(Entry) ([]byte, error)

// ReplayOption configures a Replayer.
type ReplayOption func(*Replayer)

// WithResolver supplies the resolver used for entries that reference their value by hash.
func WithResolver(fn Resolver) ReplayOption {
	return func(r *Replayer) {
		r.resolve = fn
	}
}

// Replayer strictly substitutes recorded effects for live execution.
type Replayer struct {
	mu      sync.Mutex
	entries []Entry
	pos     int
	step    int
	resolve Resolver
}

// NewReplayer initializes a replayer with a pre-recorded log.
func NewReplayer(entries []Entry, opts ...ReplayOption) *Replayer {
	r := &Replayer{
		entries: entries,
		pos:     0,
		step:    0,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SetStep updates the current step number for divergence reporting.
func (r *Replayer) SetStep(step int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.step = step
}

// Replay returns the recorded value for the requested operation.
// Fulfills FR-2.2: The underlying effect function is never invoked.
// Fulfills FR-2.3 and FR-2.4: Mismatches and exhaustion return DivergenceError.
func (r *Replayer) Replay(op, key string) ([]byte, error) {
	val, _, err := r.ReplayChunks(op, key)
	return val, err
}

// ReplayChunks replays an effect and also returns the recorded chunk sizes, nil when it was not streamed.
func (r *Replayer) ReplayChunks(op, key string) ([]byte, []int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.pos >= len(r.entries) {
		return nil, nil, &DivergenceError{
			Class:   ClassExhausted,
			Message: "run wants more effects than recorded",
			Step:    r.step,
		}
	}

	entry := r.entries[r.pos]
	if entry.Op != op || entry.Key != key {
		return nil, nil, &DivergenceError{
			Class:   ClassEffectMismatch,
			Message: "different op/key requested than recorded",
			Step:    r.step,
		}
	}

	val, err := r.value(entry)
	if err != nil {
		return nil, nil, err
	}

	r.pos++
	return val, cloneChunks(entry.Chunks), nil
}

func (r *Replayer) value(entry Entry) ([]byte, error) {
	if entry.ValueRef == "" {
		if entry.Value == nil {
			return []byte{}, nil
		}
		return entry.Value, nil
	}
	if len(entry.Value) > 0 {
		return nil, r.missing("effect carries both value and value_ref")
	}
	if r.resolve == nil {
		return nil, r.missing("effect references value " + entry.ValueRef + " but no resolver is configured")
	}
	val, err := r.resolve(entry)
	if err != nil {
		return nil, r.missing(err.Error())
	}
	return val, nil
}

func (r *Replayer) missing(msg string) error {
	return &DivergenceError{Class: ClassMissingObject, Message: msg, Step: r.step}
}
