// Package localdir provides localdir.
package localdir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

// ErrPathEscape indicates traversal.
var ErrPathEscape = errors.New("security: path traversal attempted")

// Adapter implements Source.
type Adapter struct {
	Root          string
	MaxFileSize   int64
	ExcludeGlobs  []string
	ExcludedPaths []string
}

// New creates an adapter.
func New(root string) *Adapter {
	return &Adapter{
		Root:         root,
		MaxFileSize:  10 * 1024 * 1024,
		ExcludeGlobs: []string{".git", "node_modules", "__pycache__", "build", "dist"},
	}
}

func (a *Adapter) shouldExclude(name string) bool {
	for _, g := range a.ExcludeGlobs {
		if matched, _ := filepath.Match(g, name); matched {
			return true
		}
	}
	return false
}

// Fidelity declares Tier A fidelity along with the adapter's configured
func (a *Adapter) Fidelity() source.Fidelity {
	limitations := []string{
		"symlinks and non-regular files are excluded (FR-1.10)",
		fmt.Sprintf("files larger than %d bytes are skipped (FR-7.3)", a.MaxFileSize),
	}
	if len(a.ExcludeGlobs) > 0 {
		limitations = append(limitations,
			fmt.Sprintf("paths matching exclude globs are skipped (FR-7.2): %s", strings.Join(a.ExcludeGlobs, ", ")))
	}
	return source.Fidelity{Tier: source.TierA, Limitations: limitations}
}

// Load loads a world.
func (a *Adapter) Load(ctx context.Context, w *vfs.World) error {
	a.ExcludedPaths = nil

	return filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if a.shouldExclude(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			a.ExcludedPaths = append(a.ExcludedPaths, path)
			return nil
		}

		if info.Size() > a.MaxFileSize {
			a.ExcludedPaths = append(a.ExcludedPaths, path)
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(a.Root, path)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		return w.WriteFile(rel, data, uint32(info.Mode().Perm())) //nolint:gosec
	})
}

// Apply applies a world.
func (a *Adapter) Apply(ctx context.Context, w *vfs.World) error {
	absRoot, err := filepath.Abs(a.Root)
	if err != nil {
		return err
	}

	return fs.WalkDir(w, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if path == "." || d.IsDir() {
			return nil
		}

		target := filepath.Join(absRoot, filepath.FromSlash(path))

		// Strict path traversal check
		if !strings.HasPrefix(filepath.Clean(target), absRoot+string(filepath.Separator)) && filepath.Clean(target) != absRoot {
			return ErrPathEscape
		}

		data, err := fs.ReadFile(w, path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil { //nolint:gosec
			return err
		}

		return os.WriteFile(target, data, info.Mode().Perm()) //nolint:gosec
	})
}
