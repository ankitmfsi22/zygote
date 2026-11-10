package e2b_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/source/e2b"
	"github.com/mindfire/zygote/vfs"
)

const (
	testKey      = "test-api-key-secret"
	testEnvdAuth = "test-envd-token-secret"
	testSandbox  = "sbx-123"
)

type fakeSandbox struct {
	t            *testing.T
	mu           sync.Mutex
	files        map[string][]byte
	symlinks     map[string]bool
	sizeAsString bool
	status       int
	token        string
	api          *httptest.Server
	envd         *httptest.Server
}

func newFake(t *testing.T) *fakeSandbox {
	t.Helper()
	t.Setenv("E2B_API_KEY", testKey)

	f := &fakeSandbox{
		t:        t,
		files:    map[string][]byte{},
		symlinks: map[string]bool{},
		token:    testEnvdAuth,
	}
	f.api = httptest.NewServer(http.HandlerFunc(f.handleAPI))
	f.envd = httptest.NewServer(http.HandlerFunc(f.handleEnvd))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.envd.Close)
	return f
}

func (f *fakeSandbox) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/sandboxes/"+testSandbox {
		http.Error(w, "bad route", http.StatusNotFound)
		return
	}
	if got := r.Header.Get("X-API-Key"); got != testKey {
		f.t.Errorf("X-API-Key = %q", got)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	resp := map[string]any{"sandboxID": testSandbox, "domain": nil, "envdVersion": "0.2.0"}
	if f.token != "" {
		resp["envdAccessToken"] = f.token
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		f.t.Errorf("encode: %v", err)
	}
}

func (f *fakeSandbox) checkEnvdHeaders(r *http.Request) {
	if got := r.Header.Get("E2b-Sandbox-Id"); got != testSandbox {
		f.t.Errorf("E2b-Sandbox-Id = %q", got)
	}
	if got := r.Header.Get("E2b-Sandbox-Port"); got != "49983" {
		f.t.Errorf("E2b-Sandbox-Port = %q", got)
	}
	if got := r.Header.Get("X-Access-Token"); got != f.token {
		f.t.Errorf("X-Access-Token = %q, want %q", got, f.token)
	}
}

func (f *fakeSandbox) handleEnvd(w http.ResponseWriter, r *http.Request) {
	f.checkEnvdHeaders(r)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.status != 0 {
		http.Error(w, "sandbox exploded", f.status)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/filesystem.Filesystem/ListDir":
		f.listDir(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/files":
		data, ok := f.files[r.URL.Query().Get("path")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if _, err := w.Write(data); err != nil {
			f.t.Errorf("write: %v", err)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/files":
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			f.t.Errorf("upload Content-Type = %q", ct)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("read: %v", err)
		}
		f.files[r.URL.Query().Get("path")] = data
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte(`[{"name":"x"}]`)); err != nil {
			f.t.Errorf("write: %v", err)
		}
	default:
		http.Error(w, "bad route", http.StatusNotFound)
	}
}

func (f *fakeSandbox) listDir(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("ListDir Content-Type = %q", ct)
	}
	if v := r.Header.Get("Connect-Protocol-Version"); v != "1" {
		f.t.Errorf("Connect-Protocol-Version = %q", v)
	}

	var req struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Depth != 1 {
		f.t.Errorf("depth = %d", req.Depth)
	}

	dir := strings.TrimSuffix(req.Path, "/") + "/"
	children := map[string]map[string]any{}
	add := func(full, typ string, size int) {
		if !strings.HasPrefix(full, dir) {
			return
		}
		rest := strings.TrimPrefix(full, dir)
		name, _, nested := strings.Cut(rest, "/")
		if nested {
			typ, size = "FILE_TYPE_DIRECTORY", 0
		}
		var sz any = size
		if f.sizeAsString {
			sz = fmt.Sprint(size)
		}
		children[name] = map[string]any{
			"name": name, "type": typ, "path": dir + name, "size": sz, "mode": 420,
		}
	}
	for p, data := range f.files {
		add(p, "FILE_TYPE_FILE", len(data))
	}
	for p := range f.symlinks {
		add(p, "FILE_TYPE_SYMLINK", 0)
	}

	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]map[string]any, 0, len(names))
	for _, n := range names {
		entries = append(entries, children[n])
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"entries": entries}); err != nil {
		f.t.Errorf("encode: %v", err)
	}
}

func (f *fakeSandbox) adapter(t *testing.T, root string) *e2b.Adapter {
	t.Helper()
	a, err := e2b.New(context.Background(), testSandbox, root,
		e2b.WithAPIURL(f.api.URL), e2b.WithSandboxURL(f.envd.URL))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newWorld() *vfs.World {
	return vfs.NewWorld(vfs.NewMemStore())
}

func mustRead(t *testing.T, w *vfs.World, p string) string {
	t.Helper()
	b, err := w.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestNew_MissingAPIKey(t *testing.T) {
	t.Setenv("E2B_API_KEY", "")
	if _, err := e2b.New(context.Background(), testSandbox, "/work"); err == nil {
		t.Fatal("expected error for missing E2B_API_KEY")
	}
}

func TestNew_APIErrorHidesSecrets(t *testing.T) {
	f := newFake(t)
	_, err := e2b.New(context.Background(), "other-sandbox", "/work",
		e2b.WithAPIURL(f.api.URL), e2b.WithSandboxURL(f.envd.URL))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error lacks status code: %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaks API key: %v", err)
	}
}

func TestNew_Defaults(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, "/work")
	if a.MaxFileSize != 10*1024*1024 || len(a.ExcludeGlobs) == 0 || a.Client == nil {
		t.Fatalf("unexpected defaults: %+v", a)
	}
	if a.APIURL != f.api.URL || a.SandboxURL != f.envd.URL {
		t.Fatal("options not applied")
	}
}

func TestLoadApply_RoundTrip(t *testing.T) {
	src := newFake(t)
	src.files["/work/a.txt"] = []byte("alpha")
	src.files["/work/sub/dir/b.txt"] = []byte("bravo")

	w := newWorld()
	if err := src.adapter(t, "/work").Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "a.txt"); got != "alpha" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := mustRead(t, w, "sub/dir/b.txt"); got != "bravo" {
		t.Fatalf("b.txt = %q", got)
	}

	dst := newFake(t)
	dst.files["/dest/keep.txt"] = []byte("keep")
	if err := dst.adapter(t, "/dest").Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := string(dst.files["/dest/a.txt"]); got != "alpha" {
		t.Fatalf("uploaded a.txt = %q", got)
	}
	if got := string(dst.files["/dest/sub/dir/b.txt"]); got != "bravo" {
		t.Fatalf("uploaded b.txt = %q", got)
	}
	if got := string(dst.files["/dest/keep.txt"]); got != "keep" {
		t.Fatal("Apply must not delete existing files")
	}
}

func TestLoad_SizeNumberAndString(t *testing.T) {
	for _, asString := range []bool{false, true} {
		t.Run(fmt.Sprintf("string=%v", asString), func(t *testing.T) {
			f := newFake(t)
			f.sizeAsString = asString
			f.files["/work/a.txt"] = []byte("hello")
			f.files["/work/big.bin"] = make([]byte, 100)

			a := f.adapter(t, "/work")
			a.MaxFileSize = 50
			w := newWorld()
			if err := a.Load(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, w, "a.txt"); got != "hello" {
				t.Fatalf("a.txt = %q", got)
			}
			if _, err := w.ReadFile("big.bin"); err == nil {
				t.Fatal("big.bin should be skipped")
			}
			if len(a.ExcludedPaths) != 1 || a.ExcludedPaths[0] != "/work/big.bin" {
				t.Fatalf("ExcludedPaths = %v", a.ExcludedPaths)
			}
		})
	}
}

func TestLoad_ExcludesGlobs(t *testing.T) {
	f := newFake(t)
	f.files["/work/good.txt"] = []byte("ok")
	f.files["/work/.git/config"] = []byte("secret")
	f.files["/work/node_modules/x/index.js"] = []byte("js")
	f.files["/work/sub/app.log"] = []byte("log")

	a := f.adapter(t, "/work")
	a.ExcludeGlobs = append(a.ExcludeGlobs, "*.log")
	w := newWorld()
	if err := a.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "good.txt"); got != "ok" {
		t.Fatalf("good.txt = %q", got)
	}
	for _, p := range []string{".git/config", "node_modules/x/index.js", "sub/app.log"} {
		if _, err := w.ReadFile(p); err == nil {
			t.Fatalf("%s should be excluded", p)
		}
	}
}

func TestLoad_SkipsSymlinks(t *testing.T) {
	f := newFake(t)
	f.files["/work/real.txt"] = []byte("real")
	f.symlinks["/work/link.txt"] = true

	a := f.adapter(t, "/work")
	w := newWorld()
	if err := a.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadFile("link.txt"); err == nil {
		t.Fatal("symlink should not be loaded")
	}
	if len(a.ExcludedPaths) != 1 || a.ExcludedPaths[0] != "/work/link.txt" {
		t.Fatalf("ExcludedPaths = %v", a.ExcludedPaths)
	}
}

func TestLoad_NoAccessToken(t *testing.T) {
	f := newFake(t)
	f.token = ""
	f.files["/work/a.txt"] = []byte("a")

	w := newWorld()
	if err := f.adapter(t, "/work").Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, w, "a.txt"); got != "a" {
		t.Fatalf("a.txt = %q", got)
	}
}

func TestLoadApply_NonSuccessStatus(t *testing.T) {
	f := newFake(t)
	f.files["/work/a.txt"] = []byte("a")
	a := f.adapter(t, "/work")
	f.status = http.StatusInternalServerError

	err := a.Load(context.Background(), newWorld())
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "sandbox exploded") {
		t.Fatalf("Load error = %v", err)
	}
	if strings.Contains(err.Error(), testEnvdAuth) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaks secret: %v", err)
	}

	w := newWorld()
	if err := w.WriteFile("x.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = a.Apply(context.Background(), w)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Apply error = %v", err)
	}
}

func TestApply_PathEscapeRefused(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, "/work")

	for _, rel := range []string{"../x", "../../etc/passwd", "a/../../x"} {
		if _, err := a.Resolve(rel); !errors.Is(err, e2b.ErrPathEscape) {
			t.Fatalf("Resolve(%q) error = %v, want ErrPathEscape", rel, err)
		}
	}
	got, err := a.Resolve("sub/ok.txt")
	if err != nil || got != path.Join("/work", "sub/ok.txt") {
		t.Fatalf("Resolve ok = %q, %v", got, err)
	}
}

func TestContextCancelled(t *testing.T) {
	f := newFake(t)
	f.files["/work/a.txt"] = []byte("a")
	a := f.adapter(t, "/work")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := a.Load(ctx, newWorld()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load error = %v", err)
	}

	w := newWorld()
	if err := w.WriteFile("x.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, w); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v", err)
	}
	if _, err := e2b.New(ctx, testSandbox, "/work", e2b.WithAPIURL(f.api.URL)); !errors.Is(err, context.Canceled) {
		t.Fatalf("New error = %v", err)
	}
}

func TestFidelity_TierB(t *testing.T) {
	f := newFake(t)
	fid := f.adapter(t, "/work").Fidelity()
	if fid.Tier != source.TierB {
		t.Fatalf("tier = %s", fid.Tier)
	}
	if len(fid.Limitations) < 5 {
		t.Fatalf("limitations = %v", fid.Limitations)
	}
	joined := strings.Join(fid.Limitations, "\n")
	for _, want := range []string{"step boundaries", "permissions", "symlinks", "never deletes", "larger than", "exclude globs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limitations missing %q", want)
		}
	}
}

func TestIntegration_ApplyThenLoad(t *testing.T) {
	if os.Getenv("E2B_API_KEY") == "" || os.Getenv("E2B_SANDBOX_ID") == "" {
		t.Skip("E2B_API_KEY and E2B_SANDBOX_ID not set")
	}

	ctx := context.Background()
	root := path.Join("/tmp", "zygote-e2b-test-"+fmt.Sprint(os.Getpid()))
	a, err := e2b.New(ctx, os.Getenv("E2B_SANDBOX_ID"), root)
	if err != nil {
		t.Fatal(err)
	}

	in := newWorld()
	if err := in.WriteFile("hello.txt", []byte("hello e2b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.WriteFile("nested/dir/data.txt", []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, in); err != nil {
		t.Fatal(err)
	}

	out := newWorld()
	if err := a.Load(ctx, out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"hello.txt", "nested/dir/data.txt"} {
		if got, want := mustRead(t, out, p), mustRead(t, in, p); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}
}
