package mcp

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

const (
	encodingText   = "text"
	encodingBase64 = "base64"
	defaultMode    = 0o644
)

type session struct {
	mu       sync.Mutex
	root     string
	maxRead  int
	runs     int
	runID    string
	store    *vfs.MemStore
	rec      *trace.RunRecorder
	dir      string
	active   bool
	exported bool
	poisoned bool
}

func (s *session) requireRun() error {
	if !s.active {
		return errors.New("no active run; call run.start first")
	}
	return nil
}

func (s *session) requireOpen() error {
	if err := s.requireRun(); err != nil {
		return err
	}
	if s.exported {
		return errors.New("run is closed after run.export; call run.start with force to begin a new run")
	}
	if s.poisoned {
		return errors.New("run is poisoned by an earlier failure; only run.export and read tools are allowed")
	}
	return nil
}

func (s *session) resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(s.root, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the allowed root", p)
	}
	return abs, nil
}

func (s *session) rejectSymlinks(abs string) error {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return err
	}
	cur := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink: %s", cur)
		}
	}
	return nil
}

func (s *session) stepSnapshot(n int) (trace.Step, vfs.Snapshot, error) {
	steps := s.rec.Recording().Steps
	if n < 0 || n >= len(steps) {
		return trace.Step{}, vfs.Snapshot{}, fmt.Errorf("step %d out of range (max %d)", n, len(steps)-1)
	}
	snap, err := parseHash(steps[n].Root)
	if err != nil {
		return trace.Step{}, vfs.Snapshot{}, err
	}
	return steps[n], snap, nil
}

func parseHash(s string) (vfs.Snapshot, error) {
	if len(s) != 64 {
		return vfs.Snapshot{}, fmt.Errorf("invalid world hash %q: want 64 hex characters", s)
	}
	var h vfs.Hash
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return vfs.Snapshot{}, fmt.Errorf("invalid world hash %q: %w", s, err)
	}
	return vfs.Snapshot{Root: h}, nil
}

func hashString(h vfs.Hash) string {
	return hex.EncodeToString(h[:])
}

func decodeValue(value, encoding string) ([]byte, error) {
	switch encoding {
	case "", encodingText:
		return []byte(value), nil
	case encodingBase64:
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 value: %w", err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q: want %q or %q", encoding, encodingText, encodingBase64)
	}
}

func encodeValue(b []byte) (string, string) {
	if utf8.Valid(b) {
		return string(b), encodingText
	}
	return base64.StdEncoding.EncodeToString(b), encodingBase64
}

func fileMode(m uint32) uint32 {
	if m == 0 {
		return defaultMode
	}
	return m & 0o777
}
