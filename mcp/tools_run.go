package mcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mindfire/zygote/pkg/redact"
	"github.com/mindfire/zygote/source/localdir"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStartInput is the input of run.start.
type RunStartInput struct {
	Dir   string            `json:"dir,omitempty" jsonschema:"directory under the root to seed the world from"`
	Meta  map[string]string `json:"meta,omitempty" jsonschema:"metadata attached to the run"`
	Force bool              `json:"force,omitempty" jsonschema:"replace an active run"`
}

// RunStartOutput is the output of run.start.
type RunStartOutput struct {
	RunID        string   `json:"run_id"`
	WorldHash    string   `json:"world_hash"`
	SkippedPaths []string `json:"skipped_paths"`
}

func (s *session) runStart(ctx context.Context, _ *mcpsdk.CallToolRequest, in RunStartInput) (*mcpsdk.CallToolResult, RunStartOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active && !in.Force {
		return nil, RunStartOutput{}, errors.New("a run is already active; pass force to replace it")
	}

	dir := s.root
	var abs string
	if in.Dir != "" {
		var err error
		abs, err = s.resolve(in.Dir)
		if err != nil {
			return nil, RunStartOutput{}, err
		}
		dir = abs
	}

	store := vfs.NewMemStore()
	rec := trace.Record(store)
	for k, v := range in.Meta {
		rec.Set(k, v)
	}

	skipped := []string{}
	if abs != "" {
		adapter := localdir.New(abs)
		if err := adapter.Load(ctx, rec.World()); err != nil {
			return nil, RunStartOutput{}, fmt.Errorf("loading %s: %w", in.Dir, err)
		}
		skipped = append(skipped, adapter.ExcludedPaths...)
	}
	rec.Step("init")

	s.runs++
	s.runID = fmt.Sprintf("run-%d", s.runs)
	s.store = store
	s.rec = rec
	s.dir = dir
	s.active = true
	s.exported = false
	s.poisoned = false

	return nil, RunStartOutput{RunID: s.runID, WorldHash: hashString(rec.World().Snapshot().Root), SkippedPaths: skipped}, nil
}

// RunStepInput is the input of run.step.
type RunStepInput struct {
	Name string `json:"name" jsonschema:"step name"`
}

// RunStepOutput is the output of run.step.
type RunStepOutput struct {
	N         int    `json:"n"`
	Name      string `json:"name"`
	WorldHash string `json:"world_hash"`
	Effects   int    `json:"effects"`
}

func (s *session) runStep(_ context.Context, _ *mcpsdk.CallToolRequest, in RunStepInput) (*mcpsdk.CallToolResult, RunStepOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireOpen(); err != nil {
		return nil, RunStepOutput{}, err
	}
	if in.Name == "" {
		return nil, RunStepOutput{}, errors.New("step name is required")
	}

	s.rec.Step(in.Name)
	steps := s.rec.Recording().Steps
	last := steps[len(steps)-1]
	return nil, RunStepOutput{N: last.N, Name: last.Name, WorldHash: last.Root, Effects: last.Effects}, nil
}

// RunEffectInput is the input of run.effect.
type RunEffectInput struct {
	Op       string `json:"op" jsonschema:"effect category such as model, clock, http or tool:<name>"`
	Key      string `json:"key" jsonschema:"request identity within the category"`
	Value    string `json:"value" jsonschema:"the result the agent received"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
}

// RunEffectOutput is the output of run.effect.
type RunEffectOutput struct {
	Seq int `json:"seq"`
}

func (s *session) runEffect(_ context.Context, _ *mcpsdk.CallToolRequest, in RunEffectInput) (*mcpsdk.CallToolResult, RunEffectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireOpen(); err != nil {
		return nil, RunEffectOutput{}, err
	}
	if in.Op == "" || in.Key == "" {
		return nil, RunEffectOutput{}, errors.New("op and key are required")
	}
	value, err := decodeValue(in.Value, in.Encoding)
	if err != nil {
		return nil, RunEffectOutput{}, err
	}

	if _, err := s.rec.Recorder().Record(in.Op, in.Key, func() ([]byte, error) { return value, nil }); err != nil {
		s.poisoned = true
		return nil, RunEffectOutput{}, fmt.Errorf("recording effect: %w", err)
	}
	return nil, RunEffectOutput{Seq: len(s.rec.Recorder().Entries()) - 1}, nil
}

// InputFile is a file supplied inline to run.input.
type InputFile struct {
	Path     string `json:"path" jsonschema:"slash-separated path inside the world"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
	Mode     uint32 `json:"mode,omitempty" jsonschema:"permission bits, default 420 (0644)"`
}

// RunInputInput is the input of run.input.
type RunInputInput struct {
	Name    string      `json:"name" jsonschema:"step name closing the input"`
	FromDir string      `json:"from_dir,omitempty" jsonschema:"directory under the root to load into the world"`
	Files   []InputFile `json:"files,omitempty" jsonschema:"files to write into the world"`
}

// RunInputOutput is the output of run.input.
type RunInputOutput struct {
	N            int      `json:"n"`
	WorldHash    string   `json:"world_hash"`
	SkippedPaths []string `json:"skipped_paths"`
}

func (s *session) runInput(ctx context.Context, _ *mcpsdk.CallToolRequest, in RunInputInput) (*mcpsdk.CallToolResult, RunInputOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireOpen(); err != nil {
		return nil, RunInputOutput{}, err
	}
	if in.Name == "" {
		return nil, RunInputOutput{}, errors.New("step name is required")
	}
	if in.FromDir == "" && len(in.Files) == 0 {
		return nil, RunInputOutput{}, errors.New("from_dir or files is required")
	}

	var abs string
	if in.FromDir != "" {
		var err error
		abs, err = s.resolve(in.FromDir)
		if err != nil {
			return nil, RunInputOutput{}, err
		}
	}
	decoded := make([][]byte, len(in.Files))
	for i, f := range in.Files {
		b, err := decodeValue(f.Content, f.Encoding)
		if err != nil {
			return nil, RunInputOutput{}, fmt.Errorf("file %q: %w", f.Path, err)
		}
		decoded[i] = b
	}

	skipped := []string{}
	if abs != "" {
		adapter := localdir.New(abs)
		if err := adapter.Load(ctx, s.rec.World()); err != nil {
			s.poisoned = true
			return nil, RunInputOutput{}, fmt.Errorf("loading %s: %w", in.FromDir, err)
		}
		skipped = append(skipped, adapter.ExcludedPaths...)
	}
	for i, f := range in.Files {
		if err := s.rec.World().WriteFile(f.Path, decoded[i], fileMode(f.Mode)); err != nil {
			s.poisoned = true
			return nil, RunInputOutput{}, fmt.Errorf("writing %q: %w", f.Path, err)
		}
	}

	s.rec.Step(in.Name)
	steps := s.rec.Recording().Steps
	last := steps[len(steps)-1]
	return nil, RunInputOutput{N: last.N, WorldHash: last.Root, SkippedPaths: skipped}, nil
}

// RunForkInput is the input of run.fork.
type RunForkInput struct {
	Step int    `json:"step" jsonschema:"step number to materialise"`
	Into string `json:"into" jsonschema:"target directory under the root"`
}

// RunForkOutput is the output of run.fork.
type RunForkOutput struct {
	Step         int    `json:"step"`
	Name         string `json:"name"`
	WorldHash    string `json:"world_hash"`
	FilesWritten int    `json:"files_written"`
}

func (s *session) runFork(ctx context.Context, _ *mcpsdk.CallToolRequest, in RunForkInput) (*mcpsdk.CallToolResult, RunForkOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireRun(); err != nil {
		return nil, RunForkOutput{}, err
	}
	step, snap, err := s.stepSnapshot(in.Step)
	if err != nil {
		return nil, RunForkOutput{}, err
	}
	abs, err := s.resolve(in.Into)
	if err != nil {
		return nil, RunForkOutput{}, err
	}
	if err := s.rejectSymlinks(abs); err != nil {
		return nil, RunForkOutput{}, err
	}

	world := vfs.Fork(s.store, snap)
	files := 0
	err = fs.WalkDir(world, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	if err != nil {
		return nil, RunForkOutput{}, err
	}
	if err := localdir.New(abs).Apply(ctx, world); err != nil {
		return nil, RunForkOutput{}, fmt.Errorf("materialising step %d: %w", in.Step, err)
	}
	return nil, RunForkOutput{Step: step.N, Name: step.Name, WorldHash: step.Root, FilesWritten: files}, nil
}

// RunExportInput is the input of run.export.
type RunExportInput struct {
	Out    string `json:"out" jsonschema:"bundle path under the root"`
	Policy string `json:"policy,omitempty" jsonschema:"redaction policy path under the root"`
}

// RunExportOutput is the output of run.export.
type RunExportOutput struct {
	Path          string `json:"path"`
	Steps         int    `json:"steps"`
	Effects       int    `json:"effects"`
	HasRawPrompts bool   `json:"has_raw_prompts"`
}

func (s *session) runExport(_ context.Context, _ *mcpsdk.CallToolRequest, in RunExportInput) (*mcpsdk.CallToolResult, RunExportOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireRun(); err != nil {
		return nil, RunExportOutput{}, err
	}
	out, err := s.resolve(in.Out)
	if err != nil {
		return nil, RunExportOutput{}, err
	}
	policyPath := filepath.Join(s.dir, ".zygote", "redact.yaml")
	if in.Policy != "" {
		policyPath, err = s.resolve(in.Policy)
		if err != nil {
			return nil, RunExportOutput{}, err
		}
	}

	policy, err := redact.LoadPolicy(policyPath)
	if err != nil {
		return nil, RunExportOutput{}, fmt.Errorf("loading redact policy: %w", err)
	}
	if s.poisoned {
		s.rec.Set("incomplete", "true")
	}
	bundle := &trace.Bundle{Recording: s.rec.Recording(), Store: s.store}
	clean, err := trace.RedactBundle(bundle, policy)
	if err != nil {
		return nil, RunExportOutput{}, fmt.Errorf("redaction failed (fail-closed): %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(out), ".zyg-export-*.tmp")
	if err != nil {
		return nil, RunExportOutput{}, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, RunExportOutput{}, err
	}
	if err := trace.ExportFile(tmpPath, clean.Store, clean.Recording); err != nil {
		_ = os.Remove(tmpPath)
		return nil, RunExportOutput{}, fmt.Errorf("exporting bundle: %w", err)
	}
	if err := os.Rename(tmpPath, out); err != nil {
		_ = os.Remove(tmpPath)
		return nil, RunExportOutput{}, err
	}

	s.exported = true
	return nil, RunExportOutput{
		Path:          out,
		Steps:         len(clean.Recording.Steps),
		Effects:       len(clean.Recording.Effects),
		HasRawPrompts: clean.Recording.HasRawPrompts,
	}, nil
}
