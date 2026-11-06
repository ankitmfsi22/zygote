package mcp

import (
	"context"
	"fmt"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	statusMatch    = "match"
	statusDiverged = "diverged"
	statusInvalid  = "invalid"
)

// StepHash is a step name and world hash supplied to replay.verify.
type StepHash struct {
	Name      string `json:"name"`
	WorldHash string `json:"world_hash" jsonschema:"hex world hash"`
}

// ReplayVerifyInput is the input of replay.verify.
type ReplayVerifyInput struct {
	Bundle string     `json:"bundle" jsonschema:"bundle path under the root"`
	Steps  []StepHash `json:"steps,omitempty" jsonschema:"steps after init, in order, to compare against the bundle"`
}

// RecordedStep describes one step stored in the bundle.
type RecordedStep struct {
	N         int    `json:"n"`
	Name      string `json:"name"`
	WorldHash string `json:"world_hash"`
	Effects   int    `json:"effects"`
}

// StepCheck is the verdict for one compared step.
type StepCheck struct {
	N        int    `json:"n"`
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Match    bool   `json:"match"`
}

// DivergenceInfo describes the first divergence found.
type DivergenceInfo struct {
	Class   string `json:"class"`
	Step    int    `json:"step"`
	Message string `json:"message"`
}

// ReplayVerifyOutput is the output of replay.verify.
type ReplayVerifyOutput struct {
	Valid      bool            `json:"valid"`
	Status     string          `json:"status"`
	Message    string          `json:"message,omitempty"`
	Complete   bool            `json:"complete"`
	Steps      []RecordedStep  `json:"steps"`
	Checks     []StepCheck     `json:"checks"`
	Divergence *DivergenceInfo `json:"divergence,omitempty"`
}

func (s *session) replayVerify(_ context.Context, _ *mcpsdk.CallToolRequest, in ReplayVerifyInput) (*mcpsdk.CallToolResult, ReplayVerifyOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path, err := s.resolve(in.Bundle)
	if err != nil {
		return nil, ReplayVerifyOutput{}, err
	}
	supplied := make([]vfs.Snapshot, len(in.Steps))
	for i, st := range in.Steps {
		if st.Name == "" {
			return nil, ReplayVerifyOutput{}, fmt.Errorf("steps[%d]: name is required", i)
		}
		supplied[i], err = parseHash(st.WorldHash)
		if err != nil {
			return nil, ReplayVerifyOutput{}, fmt.Errorf("steps[%d]: %w", i, err)
		}
	}

	invalid := func(err error) (*mcpsdk.CallToolResult, ReplayVerifyOutput, error) {
		return nil, ReplayVerifyOutput{Status: statusInvalid, Message: err.Error(), Steps: []RecordedStep{}, Checks: []StepCheck{}}, nil
	}

	store := vfs.NewMemStore()
	bundle, err := trace.ImportFile(path, store)
	if err != nil {
		return invalid(err)
	}
	if _, err := trace.Replay(store, &bundle); err != nil {
		return invalid(err)
	}

	recorded := bundle.Recording.Steps
	out := ReplayVerifyOutput{
		Valid:    true,
		Status:   statusMatch,
		Steps:    make([]RecordedStep, 0, len(recorded)),
		Checks:   make([]StepCheck, 0, len(supplied)),
		Complete: len(supplied) == len(recorded)-1,
	}
	for _, st := range recorded {
		out.Steps = append(out.Steps, RecordedStep{N: st.N, Name: st.Name, WorldHash: st.Root, Effects: st.Effects})
	}

	for i, st := range in.Steps {
		idx := i + 1
		div := compareStep(recorded, idx, st.Name, supplied[i], &out)
		if div != nil {
			out.Status = statusDiverged
			out.Complete = false
			out.Divergence = &DivergenceInfo{Class: string(div.Class), Step: div.Step, Message: div.Message}
			break
		}
	}
	return nil, out, nil
}

func compareStep(recorded []trace.Step, idx int, name string, actual vfs.Snapshot, out *ReplayVerifyOutput) *journal.DivergenceError {
	if idx >= len(recorded) {
		return &journal.DivergenceError{Class: journal.ClassExhausted, Message: "agent executed more steps than recorded", Step: idx}
	}
	want := recorded[idx]
	if want.Name != name {
		return &journal.DivergenceError{Class: journal.ClassStepMismatch, Message: fmt.Sprintf("expected %q, got %q", want.Name, name), Step: idx}
	}
	expected, err := parseHash(want.Root)
	if err != nil {
		return &journal.DivergenceError{Class: journal.ClassMissingObject, Message: err.Error(), Step: want.N}
	}
	chk := trace.Check{N: want.N, Name: want.Name, Expected: expected, Actual: actual, Match: expected == actual}
	out.Checks = append(out.Checks, StepCheck{N: chk.N, Name: chk.Name, Expected: hashString(chk.Expected.Root), Actual: hashString(chk.Actual.Root), Match: chk.Match})
	if !chk.Match {
		return &journal.DivergenceError{
			Class:   journal.ClassWorldMismatch,
			Message: fmt.Sprintf("world hash mismatch: expected %x, got %x", chk.Expected.Root[:], chk.Actual.Root[:]),
			Step:    chk.N,
		}
	}
	return nil
}
