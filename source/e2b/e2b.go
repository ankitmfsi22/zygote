// Package e2b provides an E2B sandbox source adapter.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

const (
	defaultAPIURL     = "https://api.e2b.app"
	defaultSandboxURL = "https://sandbox.e2b.app"
	envdPort          = "49983"
	errBodyLimit      = 4096
	defaultFileMode   = 0o644
)

const (
	fileTypeFile      = "FILE_TYPE_FILE"
	fileTypeDirectory = "FILE_TYPE_DIRECTORY"
)

// ErrPathEscape indicates traversal.
var ErrPathEscape = errors.New("security: path traversal attempted")

var errTooLarge = errors.New("response exceeds size limit")

var _ source.Source = (*Adapter)(nil)

// Adapter implements Source for an E2B sandbox.
type Adapter struct {
	Root          string
	MaxFileSize   int64
	ExcludeGlobs  []string
	ExcludedPaths []string
	APIURL        string
	SandboxURL    string
	Client        *http.Client

	sandboxID   string
	accessToken string
}

// Option configures an Adapter before it contacts E2B.
type Option func(*Adapter)

// WithAPIURL overrides the E2B control-plane URL.
func WithAPIURL(u string) Option {
	return func(a *Adapter) { a.APIURL = u }
}

// WithSandboxURL overrides the E2B sandbox (envd) URL.
func WithSandboxURL(u string) Option {
	return func(a *Adapter) { a.SandboxURL = u }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) { a.Client = c }
}

type sandboxInfo struct {
	SandboxID       string `json:"sandboxID"`
	EnvdAccessToken string `json:"envdAccessToken"`
}

type listDirRequest struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

type listDirResponse struct {
	Entries []dirEntry `json:"entries"`
}

type dirEntry struct {
	Name string      `json:"name"`
	Type string      `json:"type"`
	Path string      `json:"path"`
	Size json.Number `json:"size"`
	Mode json.Number `json:"mode"`
}

// New creates an adapter for the given sandbox and verifies it with the E2B API.
func New(ctx context.Context, sandboxID, root string, opts ...Option) (*Adapter, error) {
	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("e2b: E2B_API_KEY is not set")
	}

	a := &Adapter{
		Root:         root,
		MaxFileSize:  10 * 1024 * 1024,
		ExcludeGlobs: []string{".git", "node_modules", "__pycache__", "build", "dist"},
		APIURL:       defaultAPIURL,
		SandboxURL:   defaultSandboxURL,
		Client:       &http.Client{},
		sandboxID:    sandboxID,
	}
	for _, opt := range opts {
		opt(a)
	}

	endpoint := strings.TrimRight(a.APIURL, "/") + "/sandboxes/" + url.PathEscape(sandboxID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	data, err := a.do(req, -1)
	if err != nil {
		return nil, fmt.Errorf("e2b: get sandbox: %w", err)
	}

	var info sandboxInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("e2b: decode sandbox: %w", err)
	}
	a.accessToken = info.EnvdAccessToken

	return a, nil
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
		"state is captured only at step boundaries, not continuously",
		"file permissions are recorded on load but not applied on upload",
		"symlinks and non-regular files are skipped",
		"Apply uploads files and never deletes files from the sandbox",
		fmt.Sprintf("files larger than %d bytes are skipped (FR-7.3)", a.MaxFileSize),
	}
	if len(a.ExcludeGlobs) > 0 {
		limitations = append(limitations,
			fmt.Sprintf("paths matching exclude globs are skipped (FR-7.2): %s", strings.Join(a.ExcludeGlobs, ", ")))
	}
	return source.Fidelity{Tier: source.TierB, Limitations: limitations}
}

func (a *Adapter) do(req *http.Request, limit int64) (data []byte, err error) {
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		if rerr != nil {
			return nil, fmt.Errorf("status %d: reading body: %w", resp.StatusCode, rerr)
		}
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if limit < 0 {
		return io.ReadAll(resp.Body)
	}

	data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

func (a *Adapter) envdRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.SandboxURL, "/")+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("E2b-Sandbox-Id", a.sandboxID)
	req.Header.Set("E2b-Sandbox-Port", envdPort)
	if a.accessToken != "" {
		req.Header.Set("X-Access-Token", a.accessToken)
	}
	return req, nil
}

func (a *Adapter) listDir(ctx context.Context, dir string) ([]dirEntry, error) {
	payload, err := json.Marshal(listDirRequest{Path: dir, Depth: 1})
	if err != nil {
		return nil, err
	}

	req, err := a.envdRequest(ctx, http.MethodPost, "/filesystem.Filesystem/ListDir", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	data, err := a.do(req, -1)
	if err != nil {
		return nil, fmt.Errorf("e2b: list %s: %w", dir, err)
	}

	var res listDirResponse
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("e2b: decode listing of %s: %w", dir, err)
	}
	return res.Entries, nil
}

func (a *Adapter) download(ctx context.Context, p string) ([]byte, error) {
	req, err := a.envdRequest(ctx, http.MethodGet, "/files?"+url.Values{"path": {p}}.Encode(), nil)
	if err != nil {
		return nil, err
	}

	data, err := a.do(req, a.MaxFileSize)
	if errors.Is(err, errTooLarge) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("e2b: download %s: %w", p, err)
	}
	return data, nil
}

func (a *Adapter) upload(ctx context.Context, p string, data []byte) error {
	req, err := a.envdRequest(ctx, http.MethodPost, "/files?"+url.Values{"path": {p}}.Encode(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	if _, err := a.do(req, -1); err != nil {
		return fmt.Errorf("e2b: upload %s: %w", p, err)
	}
	return nil
}

func entryMode(e dirEntry) (uint32, error) {
	if e.Mode == "" {
		return defaultFileMode, nil
	}
	m, err := e.Mode.Int64()
	if err != nil {
		return 0, fmt.Errorf("e2b: invalid mode for %s: %w", e.Name, err)
	}
	if m < 0 || m > 0o7777 {
		return 0, fmt.Errorf("e2b: invalid mode for %s: %d", e.Name, m)
	}
	return uint32(m) & 0o777, nil //nolint:gosec
}

func (a *Adapter) walk(ctx context.Context, w *vfs.World, dir, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := a.listDir(ctx, dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		full := path.Join(dir, e.Name)
		childRel := path.Join(rel, e.Name)

		if a.shouldExclude(e.Name) {
			continue
		}

		switch e.Type {
		case fileTypeDirectory:
			if err := a.walk(ctx, w, full, childRel); err != nil {
				return err
			}
		case fileTypeFile:
			if err := a.loadFile(ctx, w, e, full, childRel); err != nil {
				return err
			}
		default:
			a.ExcludedPaths = append(a.ExcludedPaths, full)
		}
	}
	return nil
}

func (a *Adapter) loadFile(ctx context.Context, w *vfs.World, e dirEntry, full, rel string) error {
	if e.Size != "" {
		size, err := e.Size.Int64()
		if err != nil {
			return fmt.Errorf("e2b: invalid size for %s: %w", full, err)
		}
		if size > a.MaxFileSize {
			a.ExcludedPaths = append(a.ExcludedPaths, full)
			return nil
		}
	}

	mode, err := entryMode(e)
	if err != nil {
		return err
	}

	data, err := a.download(ctx, full)
	if errors.Is(err, errTooLarge) {
		a.ExcludedPaths = append(a.ExcludedPaths, full)
		return nil
	}
	if err != nil {
		return err
	}

	return w.WriteFile(rel, data, mode)
}

// Load loads a world from the sandbox.
func (a *Adapter) Load(ctx context.Context, w *vfs.World) error {
	a.ExcludedPaths = nil
	return a.walk(ctx, w, path.Clean(a.Root), "")
}

func (a *Adapter) resolve(rel string) (string, error) {
	root := path.Clean(a.Root)
	target := path.Join(root, rel)

	prefix := strings.TrimSuffix(root, "/") + "/"
	if target != root && !strings.HasPrefix(target, prefix) {
		return "", ErrPathEscape
	}
	return target, nil
}

// Apply applies a world to the sandbox without deleting any files.
func (a *Adapter) Apply(ctx context.Context, w *vfs.World) error {
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

		target, err := a.resolve(p)
		if err != nil {
			return err
		}

		data, err := fs.ReadFile(w, p)
		if err != nil {
			return err
		}

		return a.upload(ctx, target, data)
	})
}
