// Package agentos provides a Source backed by a hosted Rivet agentOS VM.
package agentos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	"time"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

const (
	defaultEndpoint = "https://api.rivet.dev"
	defaultRoot     = "/home/agentos"
	tokenEnv        = "AGENTOS_VM_TOKEN" //nolint:gosec
	endpointEnv     = "AGENTOS_ENDPOINT"
	maxResponse     = 64 << 20
	maxErrorBody    = 2048
	maxStderr       = 512
	redacted        = "[redacted]"
	applyScript     = `base64 -d < "$1" > "$2" && rm -f "$1"`
)

// ErrPathEscape indicates traversal.
var ErrPathEscape = errors.New("security: path traversal attempted")

var _ source.Source = (*Adapter)(nil)

// Option configures an Adapter.
type Option func(*Adapter)

// WithEndpoint overrides the agentOS API endpoint.
func WithEndpoint(endpoint string) Option {
	return func(a *Adapter) { a.endpoint = endpoint }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) { a.client = c }
}

// Adapter implements Source against a hosted agentOS VM.
type Adapter struct {
	Root          string
	MaxFileSize   int64
	ExcludeGlobs  []string
	ExcludedPaths []string

	vmID     string
	token    string
	endpoint string
	client   *http.Client
}

// New creates an adapter for the VM identified by vmID.
func New(_ context.Context, vmID, root string, opts ...Option) (*Adapter, error) {
	if vmID == "" {
		return nil, errors.New("agentos: vm id is required")
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil, fmt.Errorf("agentos: %s is not set", tokenEnv)
	}
	endpoint := os.Getenv(endpointEnv)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if root == "" {
		root = defaultRoot
	}
	a := &Adapter{
		Root:         root,
		MaxFileSize:  10 * 1024 * 1024,
		ExcludeGlobs: []string{".git", "node_modules", "__pycache__", "build", "dist"},
		vmID:         vmID,
		token:        token,
		endpoint:     endpoint,
		client:       &http.Client{Timeout: 5 * time.Minute},
	}
	for _, opt := range opts {
		opt(a)
	}
	a.endpoint = strings.TrimRight(a.endpoint, "/")
	if a.endpoint == "" {
		return nil, errors.New("agentos: endpoint is empty")
	}
	if a.client == nil {
		return nil, errors.New("agentos: http client is nil")
	}
	return a, nil
}

// Fidelity declares Tier B fidelity along with the adapter's configured limits.
func (a *Adapter) Fidelity() source.Fidelity {
	limitations := []string{
		"step-boundary capture only; no continuous recording",
		"files are transferred as base64 via process.run and require base64 in the VM",
		"file mode and ownership are not captured or applied",
		"symlinks and non-regular files are skipped (FR-1.10)",
		"Apply never deletes files from the VM",
		fmt.Sprintf("files larger than %d bytes are skipped (FR-7.3)", a.MaxFileSize),
		"VM process and memory state are not captured",
	}
	if len(a.ExcludeGlobs) > 0 {
		limitations = append(limitations,
			fmt.Sprintf("paths matching exclude globs are skipped (FR-7.2): %s", strings.Join(a.ExcludeGlobs, ", ")))
	}
	return source.Fidelity{Tier: source.TierB, Limitations: limitations}
}

type dirEntry struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	SizeBytes int64  `json:"sizeBytes"`
}

type runOutput struct {
	Status struct {
		ExitCode int `json:"exitCode"`
	} `json:"status"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type apiError struct {
	Code    string `json:"code"`
	Group   string `json:"group"`
	Message string `json:"message"`
}

func (a *Adapter) root() string {
	r := a.Root
	if r == "" {
		r = defaultRoot
	}
	return path.Clean(r)
}

func (a *Adapter) redact(s string) string {
	return strings.ReplaceAll(s, a.token, redacted)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (a *Adapter) call(ctx context.Context, action string, input, output any) error {
	body, err := json.Marshal(map[string][]any{"args": {input}})
	if err != nil {
		return fmt.Errorf("agentos: %s: encode request: %w", action, err)
	}
	endpoint := a.endpoint + "/gateway/" + url.PathEscape(a.vmID) + "/action/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		return fmt.Errorf("agentos: %s: build request: %s", action, a.redact(err.Error()))
	}
	req.Header.Set("x-rivet-token", a.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec
	if err != nil {
		return fmt.Errorf("agentos: %s: request failed: %s", action, a.redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && (ae.Code != "" || ae.Message != "") {
			return fmt.Errorf("agentos: %s: status %d: %s/%s: %s", action, resp.StatusCode,
				a.redact(ae.Group), a.redact(ae.Code), a.redact(truncate(ae.Message, maxErrorBody)))
		}
		return fmt.Errorf("agentos: %s: status %d: %s", action, resp.StatusCode, a.redact(truncate(string(raw), maxErrorBody)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("agentos: %s: read response: %s", action, a.redact(err.Error()))
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("agentos: %s: response exceeds %d bytes", action, maxResponse)
	}
	if output == nil {
		return nil
	}
	envelope := struct {
		Output json.RawMessage `json:"output"`
	}{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("agentos: %s: decode response: %w", action, err)
	}
	if err := json.Unmarshal(envelope.Output, output); err != nil {
		return fmt.Errorf("agentos: %s: decode output: %w", action, err)
	}
	return nil
}

func (a *Adapter) run(ctx context.Context, command string, args ...string) (string, error) {
	if args == nil {
		args = []string{}
	}
	input := map[string]any{
		"command": command,
		"args":    args,
		"options": map[string]any{"captureStdio": true},
	}
	var out runOutput
	if err := a.call(ctx, "v1.process.run", input, &out); err != nil {
		return "", err
	}
	if out.Status.ExitCode != 0 {
		return "", fmt.Errorf("agentos: %s exited with code %d: %s", command, out.Status.ExitCode,
			a.redact(truncate(strings.TrimSpace(out.Stderr), maxStderr)))
	}
	return out.Stdout, nil
}

func (a *Adapter) shouldExclude(name string) bool {
	for _, g := range a.ExcludeGlobs {
		if matched, _ := path.Match(g, name); matched {
			return true
		}
	}
	return false
}

func (a *Adapter) excludedByAncestor(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if a.shouldExclude(part) {
			return true
		}
	}
	return false
}

// Load loads the VM filesystem under Root into the world.
func (a *Adapter) Load(ctx context.Context, w *vfs.World) error {
	a.ExcludedPaths = nil
	root := a.root()

	exclude := a.ExcludeGlobs
	if exclude == nil {
		exclude = []string{}
	}
	var entries []dirEntry
	if err := a.call(ctx, "v1.filesystem.readdirRecursive", map[string]any{"path": root, "exclude": exclude}, &entries); err != nil {
		return err
	}

	prefix := strings.TrimRight(root, "/") + "/"
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type == "directory" {
			continue
		}
		p := path.Clean(e.Path)
		if !strings.HasPrefix(p, "/") {
			p = path.Join(root, p)
		}
		if !strings.HasPrefix(p, prefix) {
			a.ExcludedPaths = append(a.ExcludedPaths, p)
			continue
		}
		rel := strings.TrimPrefix(p, prefix)
		if a.excludedByAncestor(rel) {
			continue
		}
		if e.Type != "file" || e.SizeBytes > a.MaxFileSize {
			a.ExcludedPaths = append(a.ExcludedPaths, p)
			continue
		}

		stdout, err := a.run(ctx, "base64", p)
		if err != nil {
			return err
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(stdout), ""))
		if err != nil {
			return fmt.Errorf("agentos: decode %s: %w", p, err)
		}
		if err := w.WriteFile(rel, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func resolveTarget(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrPathEscape
	}
	target := path.Join(root, rel)
	if !strings.HasPrefix(target, strings.TrimRight(root, "/")+"/") {
		return "", ErrPathEscape
	}
	return target, nil
}

func tempName(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return path.Join(dir, ".zygote-"+hex.EncodeToString(b[:])+".b64"), nil
}

// Apply writes the world's files into the VM under Root.
func (a *Adapter) Apply(ctx context.Context, w *vfs.World) error {
	root := a.root()

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

		target, err := resolveTarget(root, p)
		if err != nil {
			return err
		}

		data, err := fs.ReadFile(w, p)
		if err != nil {
			return err
		}

		dir := path.Dir(target)
		if err := a.call(ctx, "v1.filesystem.mkdir", map[string]any{"path": dir, "recursive": true}, nil); err != nil {
			return err
		}
		tmp, err := tempName(dir)
		if err != nil {
			return err
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		if err := a.call(ctx, "v1.filesystem.writeFile", map[string]any{"path": tmp, "content": encoded}, nil); err != nil {
			return err
		}
		_, err = a.run(ctx, "sh", "-c", applyScript, "sh", tmp, target)
		return err
	})
}
