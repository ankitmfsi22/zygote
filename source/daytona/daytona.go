// Package daytona provides a Daytona sandbox source adapter.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	sdk "github.com/daytona/clients/sdk-go/pkg/daytona"
	"github.com/daytona/clients/sdk-go/pkg/types"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

// ErrPathEscape indicates traversal.
var ErrPathEscape = errors.New("security: path traversal attempted")

var _ source.Source = (*Adapter)(nil)

// FileSystem is the subset of the Daytona sandbox filesystem the adapter uses.
type FileSystem interface {
	ListFiles(ctx context.Context, dir string) ([]*types.FileInfo, error)
	DownloadFile(ctx context.Context, remotePath string) ([]byte, error)
	UploadFile(ctx context.Context, data []byte, remotePath string) error
	CreateFolder(ctx context.Context, dir string) error
}

type sdkFileSystem struct {
	fs *sdk.FileSystemService
}

func (s sdkFileSystem) ListFiles(ctx context.Context, dir string) ([]*types.FileInfo, error) {
	return s.fs.ListFiles(ctx, dir)
}

func (s sdkFileSystem) DownloadFile(ctx context.Context, remotePath string) ([]byte, error) {
	return s.fs.DownloadFile(ctx, remotePath, nil)
}

func (s sdkFileSystem) UploadFile(ctx context.Context, data []byte, remotePath string) error {
	return s.fs.UploadFile(ctx, data, remotePath)
}

func (s sdkFileSystem) CreateFolder(ctx context.Context, dir string) error {
	return s.fs.CreateFolder(ctx, dir)
}

// Adapter implements Source against a Daytona sandbox.
type Adapter struct {
	Root          string
	MaxFileSize   int64
	ExcludeGlobs  []string
	ExcludedPaths []string

	fs FileSystem
}

// New creates an adapter for a sandbox using DAYTONA_API_KEY and DAYTONA_API_URL.
func New(ctx context.Context, sandboxIDOrName, root string) (*Adapter, error) {
	client, err := sdk.NewClient()
	if err != nil {
		return nil, fmt.Errorf("daytona client: %w", err)
	}

	sandbox, err := client.Get(ctx, sandboxIDOrName)
	if err != nil {
		return nil, fmt.Errorf("daytona sandbox %q: %w", sandboxIDOrName, err)
	}

	return NewWithFileSystem(sdkFileSystem{fs: sandbox.FileSystem}, root), nil
}

// NewWithFileSystem creates an adapter over the given filesystem.
func NewWithFileSystem(fsys FileSystem, root string) *Adapter {
	return &Adapter{
		Root:         path.Clean(root),
		MaxFileSize:  10 * 1024 * 1024,
		ExcludeGlobs: []string{".git", "node_modules", "__pycache__", "build", "dist"},
		fs:           fsys,
	}
}

func (a *Adapter) shouldExclude(name string) bool {
	for _, g := range a.ExcludeGlobs {
		if matched, _ := path.Match(g, name); matched {
			return true
		}
	}
	return false
}

// Fidelity declares Tier B fidelity along with the adapter's configured limits.
func (a *Adapter) Fidelity() source.Fidelity {
	limitations := []string{
		"state is captured at step boundaries only; changes within a step are invisible",
		"file permissions are not applied on write; the sandbox default applies",
		"symlinks are not distinguished by the SDK and may be followed or skipped",
		"Apply never deletes files in the sandbox",
		fmt.Sprintf("files larger than %d bytes are skipped (FR-7.3)", a.MaxFileSize),
	}
	if len(a.ExcludeGlobs) > 0 {
		limitations = append(limitations,
			fmt.Sprintf("paths matching exclude globs are skipped (FR-7.2): %s", strings.Join(a.ExcludeGlobs, ", ")))
	}
	return source.Fidelity{Tier: source.TierB, Limitations: limitations}
}

// Load loads a world from the sandbox.
func (a *Adapter) Load(ctx context.Context, w *vfs.World) error {
	a.ExcludedPaths = nil
	return a.walk(ctx, a.Root, w)
}

func (a *Adapter) walk(ctx context.Context, dir string, w *vfs.World) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := a.fs.ListFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		if a.shouldExclude(e.Name) {
			continue
		}

		full := path.Join(dir, e.Name)

		if e.IsDirectory {
			if err := a.walk(ctx, full, w); err != nil {
				return err
			}
			continue
		}

		if e.Size > a.MaxFileSize {
			a.ExcludedPaths = append(a.ExcludedPaths, full)
			continue
		}

		data, err := a.fs.DownloadFile(ctx, full)
		if err != nil {
			return fmt.Errorf("download %s: %w", full, err)
		}

		if int64(len(data)) > a.MaxFileSize {
			a.ExcludedPaths = append(a.ExcludedPaths, full)
			continue
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(full, a.Root), "/")
		if err := w.WriteFile(rel, data, parseMode(e.Mode)); err != nil {
			return err
		}
	}

	return nil
}

func parseMode(s string) uint32 {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0644
	}
	return uint32(m) & 0777 //nolint:gosec
}

func (a *Adapter) target(rel string) (string, error) {
	target := path.Join(a.Root, rel)
	prefix := a.Root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if target != a.Root && !strings.HasPrefix(target, prefix) {
		return "", ErrPathEscape
	}
	return target, nil
}

// Apply writes a world into the sandbox.
func (a *Adapter) Apply(ctx context.Context, w *vfs.World) error {
	created := map[string]bool{}

	return fs.WalkDir(w, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if p == "." || d.IsDir() {
			return nil
		}

		target, err := a.target(p)
		if err != nil {
			return err
		}

		data, err := fs.ReadFile(w, p)
		if err != nil {
			return err
		}

		if err := a.ensureDir(ctx, path.Dir(target), created); err != nil {
			return err
		}

		if err := a.fs.UploadFile(ctx, data, target); err != nil {
			return fmt.Errorf("upload %s: %w", target, err)
		}
		return nil
	})
}

func (a *Adapter) ensureDir(ctx context.Context, dir string, created map[string]bool) error {
	if created[dir] || dir == "/" || dir == "." {
		return nil
	}

	if parent := path.Dir(dir); parent != dir {
		if err := a.ensureDir(ctx, parent, created); err != nil {
			return err
		}
	}

	if err := a.fs.CreateFolder(ctx, dir); err != nil {
		return fmt.Errorf("create folder %s: %w", dir, err)
	}
	created[dir] = true
	return nil
}
