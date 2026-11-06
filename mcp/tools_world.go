package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/mindfire/zygote/vfs"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const liveWorld = "live"

// WorldReadInput is the input of world.read.
type WorldReadInput struct {
	Path string `json:"path" jsonschema:"slash-separated path, or . for the root directory"`
	Step *int   `json:"step,omitempty" jsonschema:"recorded step to read instead of the live world"`
}

// DirEntryInfo describes one entry of a listed directory.
type DirEntryInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// WorldReadOutput is the output of world.read.
type WorldReadOutput struct {
	Kind      string         `json:"kind"`
	Content   string         `json:"content,omitempty"`
	Encoding  string         `json:"encoding,omitempty"`
	Size      int64          `json:"size"`
	Mode      uint32         `json:"mode"`
	Truncated bool           `json:"truncated"`
	Entries   []DirEntryInfo `json:"entries,omitempty"`
}

func (s *session) worldRead(_ context.Context, _ *mcpsdk.CallToolRequest, in WorldReadInput) (*mcpsdk.CallToolResult, WorldReadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireRun(); err != nil {
		return nil, WorldReadOutput{}, err
	}
	world := s.rec.World()
	if in.Step != nil {
		_, snap, err := s.stepSnapshot(*in.Step)
		if err != nil {
			return nil, WorldReadOutput{}, err
		}
		world = vfs.Fork(s.store, snap)
	}

	info, err := world.Stat(in.Path)
	if err != nil {
		return nil, WorldReadOutput{}, err
	}
	out := WorldReadOutput{Size: info.Size(), Mode: uint32(info.Mode().Perm())}

	if info.IsDir() {
		entries, err := world.ReadDir(in.Path)
		if err != nil {
			return nil, WorldReadOutput{}, err
		}
		out.Kind = "dir"
		out.Entries = make([]DirEntryInfo, 0, len(entries))
		for _, e := range entries {
			ei, err := e.Info()
			if err != nil {
				return nil, WorldReadOutput{}, err
			}
			kind := "file"
			if e.IsDir() {
				kind = "dir"
			}
			out.Entries = append(out.Entries, DirEntryInfo{Name: e.Name(), Kind: kind, Size: ei.Size()})
		}
		return nil, out, nil
	}

	data, err := world.ReadFile(in.Path)
	if err != nil {
		return nil, WorldReadOutput{}, err
	}
	if len(data) > s.maxRead {
		data = data[:s.maxRead]
		out.Truncated = true
	}
	out.Kind = "file"
	out.Content, out.Encoding = encodeValue(data)
	return nil, out, nil
}

// WorldWriteInput is the input of world.write.
type WorldWriteInput struct {
	Path     string `json:"path" jsonschema:"slash-separated path inside the world"`
	Content  string `json:"content,omitempty"`
	Encoding string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
	Mode     uint32 `json:"mode,omitempty" jsonschema:"permission bits, default 420 (0644)"`
	Delete   bool   `json:"delete,omitempty" jsonschema:"remove the path instead of writing it"`
}

// WorldWriteOutput is the output of world.write.
type WorldWriteOutput struct {
	WorldHash string `json:"world_hash"`
}

func (s *session) worldWrite(_ context.Context, _ *mcpsdk.CallToolRequest, in WorldWriteInput) (*mcpsdk.CallToolResult, WorldWriteOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireOpen(); err != nil {
		return nil, WorldWriteOutput{}, err
	}
	world := s.rec.World()
	if in.Delete {
		if err := world.Remove(in.Path); err != nil {
			return nil, WorldWriteOutput{}, fmt.Errorf("removing %q: %w", in.Path, err)
		}
	} else {
		data, err := decodeValue(in.Content, in.Encoding)
		if err != nil {
			return nil, WorldWriteOutput{}, err
		}
		if err := world.WriteFile(in.Path, data, fileMode(in.Mode)); err != nil {
			return nil, WorldWriteOutput{}, fmt.Errorf("writing %q: %w", in.Path, err)
		}
	}
	return nil, WorldWriteOutput{WorldHash: hashString(world.Snapshot().Root)}, nil
}

// WorldDiffInput is the input of world.diff.
type WorldDiffInput struct {
	From string `json:"from" jsonschema:"step number or live"`
	To   string `json:"to" jsonschema:"step number or live"`
}

// ChangeInfo describes one changed path.
type ChangeInfo struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// WorldDiffOutput is the output of world.diff.
type WorldDiffOutput struct {
	Changes []ChangeInfo `json:"changes"`
}

func (s *session) diffSnapshot(ref string) (vfs.Snapshot, error) {
	if ref == liveWorld {
		return s.rec.World().Snapshot(), nil
	}
	n, err := strconv.Atoi(ref)
	if err != nil {
		return vfs.Snapshot{}, fmt.Errorf("invalid step %q: want a step number or %q", ref, liveWorld)
	}
	_, snap, err := s.stepSnapshot(n)
	return snap, err
}

func (s *session) worldDiff(_ context.Context, _ *mcpsdk.CallToolRequest, in WorldDiffInput) (*mcpsdk.CallToolResult, WorldDiffOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.requireRun(); err != nil {
		return nil, WorldDiffOutput{}, err
	}
	from, err := s.diffSnapshot(in.From)
	if err != nil {
		return nil, WorldDiffOutput{}, err
	}
	to, err := s.diffSnapshot(in.To)
	if err != nil {
		return nil, WorldDiffOutput{}, err
	}

	changes, err := vfs.Diff(s.store, from, to)
	if err != nil {
		return nil, WorldDiffOutput{}, fmt.Errorf("computing diff: %w", err)
	}
	out := WorldDiffOutput{Changes: make([]ChangeInfo, 0, len(changes))}
	for _, c := range changes {
		ci := ChangeInfo{Path: c.Path}
		switch c.Kind {
		case vfs.Added:
			ci.Kind = "added"
			ci.To = hashString(c.To)
		case vfs.Deleted:
			ci.Kind = "deleted"
			ci.From = hashString(c.From)
		case vfs.Modified:
			ci.Kind = "modified"
			ci.From = hashString(c.From)
			ci.To = hashString(c.To)
		default:
			return nil, WorldDiffOutput{}, errors.New("unclassifiable change kind")
		}
		out.Changes = append(out.Changes, ci)
	}
	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Path < out.Changes[j].Path })
	return nil, out, nil
}
