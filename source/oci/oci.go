// Package oci provides a Source backed by a container's Docker-compatible Engine API.
package oci

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

const (
	defaultHost  = "unix:///var/run/docker.sock"
	unixBaseURL  = "http://docker"
	maxErrorBody = 4096
)

// ErrPathEscape indicates traversal.
var ErrPathEscape = errors.New("security: path traversal attempted")

var _ source.Source = (*Adapter)(nil)

// Adapter implements Source.
type Adapter struct {
	Root          string
	MaxFileSize   int64
	ExcludeGlobs  []string
	ExcludedPaths []string

	containerID string
	client      *http.Client
	baseURL     string
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithHTTPClient sets the HTTP client used to reach the engine.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) { a.client = c }
}

// WithBaseURL sets the engine base URL, overriding DOCKER_HOST.
func WithBaseURL(u string) Option {
	return func(a *Adapter) { a.baseURL = strings.TrimRight(u, "/") }
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("oci: engine returned status %d: %s", e.Status, e.Message)
}

// New creates an adapter and verifies that the container exists.
func New(ctx context.Context, containerID, root string, opts ...Option) (*Adapter, error) {
	if containerID == "" {
		return nil, errors.New("oci: container id is required")
	}
	if root == "" {
		return nil, errors.New("oci: root path is required")
	}

	a := &Adapter{
		Root:         root,
		MaxFileSize:  10 * 1024 * 1024,
		ExcludeGlobs: []string{".git", "node_modules", "__pycache__", "build", "dist"},
		containerID:  containerID,
	}
	for _, opt := range opts {
		opt(a)
	}

	if a.client == nil && a.baseURL != "" {
		a.client = http.DefaultClient
	}
	if a.client == nil || a.baseURL == "" {
		c, b, err := connect(resolveHost())
		if err != nil {
			return nil, err
		}
		if a.client == nil {
			a.client = c
		}
		if a.baseURL == "" {
			a.baseURL = b
		}
	}

	resp, err := a.request(ctx, http.MethodGet, a.containerEndpoint("json"), nil, nil, "")
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("oci: container %q not found: %s", containerID, ae.Message)
		}
		return nil, fmt.Errorf("oci: inspecting container %q: %w", containerID, err)
	}
	defer closeBody(resp.Body)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))

	return a, nil
}

func resolveHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return defaultHost
}

func connect(host string) (*http.Client, string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, "", fmt.Errorf("oci: invalid DOCKER_HOST %q: %w", host, err)
	}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		if sock == "" {
			return nil, "", fmt.Errorf("oci: invalid DOCKER_HOST %q: missing socket path", host)
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
		return &http.Client{Transport: transport}, unixBaseURL, nil
	case "tcp":
		if u.Host == "" {
			return nil, "", fmt.Errorf("oci: invalid DOCKER_HOST %q: missing host", host)
		}
		return &http.Client{}, "http://" + u.Host, nil
	default:
		return nil, "", fmt.Errorf("oci: unsupported DOCKER_HOST scheme %q", u.Scheme)
	}
}

func closeBody(b io.Closer) {
	_ = b.Close()
}

func (a *Adapter) containerEndpoint(suffix string) string {
	return "/containers/" + url.PathEscape(a.containerID) + "/" + suffix
}

func (a *Adapter) archiveQuery() url.Values {
	return url.Values{"path": {a.Root}}
}

func (a *Adapter) request(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	target := a.baseURL + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("oci: building request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := a.client.Do(req) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("oci: %s %s: %w", method, endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer closeBody(resp.Body)
		return nil, readAPIError(resp)
	}
	return resp, nil
}

func readAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var payload struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(raw))
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Message != "" {
		msg = payload.Message
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &apiError{Status: resp.StatusCode, Message: msg}
}

// Fidelity declares Tier B fidelity along with the adapter's limitations.
func (a *Adapter) Fidelity() source.Fidelity {
	limitations := []string{
		"state is captured at step boundaries only; no continuous observation",
		"symlinks, hard links and other non-regular files are skipped (FR-1.10)",
		"Apply never deletes files from the container",
		"file ownership (uid/gid) is not preserved",
		"container process and memory state are not captured",
		fmt.Sprintf("files larger than %d bytes are skipped (FR-7.3)", a.MaxFileSize),
	}
	if len(a.ExcludeGlobs) > 0 {
		limitations = append(limitations,
			fmt.Sprintf("paths matching exclude globs are skipped (FR-7.2): %s", strings.Join(a.ExcludeGlobs, ", ")))
	}
	return source.Fidelity{Tier: source.TierB, Limitations: limitations}
}

func (a *Adapter) shouldExclude(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		for _, g := range a.ExcludeGlobs {
			if matched, _ := path.Match(g, part); matched {
				return true
			}
		}
	}
	return false
}

func stripRoot(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrPathEscape, name)
		}
	}
	_, rel, ok := strings.Cut(path.Clean(name), "/")
	if !ok {
		return "", nil
	}
	return rel, nil
}

func checkPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("%w: %q", ErrPathEscape, p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("%w: %q", ErrPathEscape, p)
		}
	}
	return nil
}

// Load loads a world.
func (a *Adapter) Load(ctx context.Context, w *vfs.World) error {
	a.ExcludedPaths = nil

	resp, err := a.request(ctx, http.MethodGet, a.containerEndpoint("archive"), a.archiveQuery(), nil, "")
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)

	tr := tar.NewReader(resp.Body)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("oci: reading archive: %w", err)
		}
		if err := a.loadEntry(tr, hdr, w); err != nil {
			return err
		}
	}
}

func (a *Adapter) loadEntry(tr *tar.Reader, hdr *tar.Header, w *vfs.World) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}

	rel, err := stripRoot(hdr.Name)
	if err != nil {
		return err
	}
	if rel == "" || a.shouldExclude(rel) {
		return nil
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return nil
	case tar.TypeReg:
	default:
		a.ExcludedPaths = append(a.ExcludedPaths, rel)
		return nil
	}

	if hdr.Size > a.MaxFileSize {
		a.ExcludedPaths = append(a.ExcludedPaths, rel)
		return nil
	}

	data, err := io.ReadAll(tr)
	if err != nil {
		return fmt.Errorf("oci: reading %q: %w", rel, err)
	}
	return w.WriteFile(rel, data, uint32(hdr.FileInfo().Mode().Perm()))
}

// Apply applies a world.
func (a *Adapter) Apply(ctx context.Context, w *vfs.World) error {
	err := fs.WalkDir(w, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		return checkPath(p)
	})
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		werr := writeTar(ctx, pw, w)
		_ = pw.CloseWithError(werr)
		done <- werr
	}()

	resp, err := a.request(ctx, http.MethodPut, a.containerEndpoint("archive"), a.archiveQuery(), pr, "application/x-tar")
	_ = pr.Close()
	werr := <-done
	if resp != nil {
		defer closeBody(resp.Body)
	}

	var ae *apiError
	if errors.As(err, &ae) {
		return err
	}
	if werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		return fmt.Errorf("oci: building archive: %w", werr)
	}
	return err
}

func writeTar(ctx context.Context, out io.Writer, w *vfs.World) error {
	tw := tar.NewWriter(out)
	err := fs.WalkDir(w, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := int64(info.Mode().Perm())

		data, err := w.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: perm, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
