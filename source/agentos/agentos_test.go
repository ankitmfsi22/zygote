package agentos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

const testToken = "tok-SECRET-123"

type recorded struct {
	path   string
	header http.Header
	body   []byte
}

type fakeVM struct {
	t        *testing.T
	mu       sync.Mutex
	files    map[string][]byte
	symlinks map[string]bool
	requests []recorded
	failRun  bool
	failHTTP bool
	scripts  []string
}

func newFakeVM(t *testing.T) (*fakeVM, *Adapter) {
	t.Helper()
	t.Setenv("AGENTOS_VM_TOKEN", testToken)
	t.Setenv("AGENTOS_ENDPOINT", "")
	vm := &fakeVM{t: t, files: map[string][]byte{}, symlinks: map[string]bool{}}
	srv := httptest.NewServer(vm)
	t.Cleanup(srv.Close)
	a, err := New(context.Background(), "vm-1", "/home/agentos", WithEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return vm, a
}

func (vm *fakeVM) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	vm.requests = append(vm.requests, recorded{path: r.URL.Path, header: r.Header.Clone(), body: buf.Bytes()})

	reply := func(v any) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"output": v})
	}
	if vm.failHTTP {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(`{"code":"boom","group":"actor","message":"failed with ` + testToken + `"}`))
		return
	}

	var req struct {
		Args []json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(buf.Bytes(), &req); err != nil || len(req.Args) != 1 {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	action := path.Base(r.URL.Path)

	switch action {
	case "v1.filesystem.readdirRecursive":
		var in struct {
			Path    string   `json:"path"`
			Exclude []string `json:"exclude"`
		}
		_ = json.Unmarshal(req.Args[0], &in)
		out := []map[string]any{{"path": in.Path + "/sub", "type": "directory", "sizeBytes": 0}}
		for p, data := range vm.files {
			if !strings.HasPrefix(p, in.Path+"/") || vm.excluded(p, in.Exclude) {
				continue
			}
			out = append(out, map[string]any{"path": p, "type": "file", "sizeBytes": len(data)})
		}
		for p := range vm.symlinks {
			if strings.HasPrefix(p, in.Path+"/") {
				out = append(out, map[string]any{"path": p, "type": "symlink", "sizeBytes": 0})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["path"].(string) < out[j]["path"].(string) })
		reply(out)
	case "v1.filesystem.mkdir":
		reply(nil)
	case "v1.filesystem.writeFile":
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(req.Args[0], &in)
		vm.files[in.Path] = []byte(in.Content)
		reply(nil)
	case "v1.process.run":
		vm.handleRun(req.Args[0], reply)
	default:
		rw.WriteHeader(http.StatusNotFound)
	}
}

func (vm *fakeVM) excluded(p string, globs []string) bool {
	for _, part := range strings.Split(p, "/") {
		for _, g := range globs {
			if m, _ := path.Match(g, part); m {
				return true
			}
		}
	}
	return false
}

func (vm *fakeVM) handleRun(raw json.RawMessage, reply func(any)) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Options struct {
			CaptureStdio bool `json:"captureStdio"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &in)
	result := func(code int, stdout, stderr string) {
		reply(map[string]any{"status": map[string]any{"exitCode": code}, "stdout": stdout, "stderr": stderr})
	}
	if vm.failRun || !in.Options.CaptureStdio {
		result(1, "", "run failed "+testToken)
		return
	}
	switch in.Command {
	case "base64":
		data, ok := vm.files[in.Args[0]]
		if !ok {
			result(1, "", "no such file")
			return
		}
		enc := base64.StdEncoding.EncodeToString(data)
		var sb strings.Builder
		for len(enc) > 76 {
			sb.WriteString(enc[:76] + "\n")
			enc = enc[76:]
		}
		sb.WriteString(enc + "\n")
		result(0, sb.String(), "")
	case "sh":
		if len(in.Args) != 5 || in.Args[0] != "-c" || in.Args[2] != "sh" {
			result(2, "", "bad sh args")
			return
		}
		vm.scripts = append(vm.scripts, in.Args[1])
		tmp, target := in.Args[3], in.Args[4]
		data, err := base64.StdEncoding.DecodeString(string(vm.files[tmp]))
		if err != nil {
			result(1, "", "decode failed")
			return
		}
		vm.files[target] = data
		delete(vm.files, tmp)
		result(0, "", "")
	default:
		result(127, "", "not found")
	}
}

func newWorld() *vfs.World {
	return vfs.NewWorld(vfs.NewMemStore())
}

var binary = []byte{0x00, 0xff, 0x01, 0xfe, 'a', 0x00, 0x80}

func TestSourceInterfaceAndRequestShape(t *testing.T) {
	vm, a := newFakeVM(t)
	var _ source.Source = a
	vm.files["/home/agentos/a.txt"] = []byte("hi")

	if err := a.Load(context.Background(), newWorld()); err != nil {
		t.Fatal(err)
	}
	if len(vm.requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(vm.requests))
	}
	first := vm.requests[0]
	if first.path != "/gateway/vm-1/action/v1.filesystem.readdirRecursive" {
		t.Fatalf("unexpected path %q", first.path)
	}
	if first.header.Get("x-rivet-token") != testToken {
		t.Fatal("missing token header")
	}
	if first.header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected content type %q", first.header.Get("Content-Type"))
	}
	var body struct {
		Args []map[string]any `json:"args"`
	}
	if err := json.Unmarshal(first.body, &body); err != nil || len(body.Args) != 1 {
		t.Fatalf("bad body %s", first.body)
	}
	if body.Args[0]["path"] != "/home/agentos" {
		t.Fatalf("unexpected path arg %v", body.Args[0]["path"])
	}
	if _, ok := body.Args[0]["exclude"].([]any); !ok {
		t.Fatalf("exclude must be an array: %v", body.Args[0]["exclude"])
	}
	if vm.requests[1].path != "/gateway/vm-1/action/v1.process.run" {
		t.Fatalf("unexpected path %q", vm.requests[1].path)
	}
}

func TestRoundTripBinary(t *testing.T) {
	vm, a := newFakeVM(t)
	w := newWorld()
	if err := w.WriteFile("bin/data.bin", binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("empty.txt", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vm.files["/home/agentos/bin/data.bin"], binary) {
		t.Fatalf("apply wrote %v", vm.files["/home/agentos/bin/data.bin"])
	}
	for p := range vm.files {
		if strings.Contains(p, ".zygote-") {
			t.Fatalf("temp file left behind: %s", p)
		}
	}

	loaded := newWorld()
	if err := a.Load(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	got, err := loaded.ReadFile("bin/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("round trip mismatch: %v", got)
	}
	if _, err := loaded.ReadFile("empty.txt"); err != nil {
		t.Fatalf("empty file missing: %v", err)
	}
}

func TestApplyPassesPathsAsPositionalArgs(t *testing.T) {
	vm, a := newFakeVM(t)
	w := newWorld()
	if err := w.WriteFile("dir/we ird$(x)\"name.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if len(vm.scripts) != 1 {
		t.Fatalf("expected 1 script, got %d", len(vm.scripts))
	}
	script := vm.scripts[0]
	for _, bad := range []string{"/home/agentos", "weird", "we ird", ".zygote-", "name.txt"} {
		if strings.Contains(script, bad) {
			t.Fatalf("script contains path fragment %q: %s", bad, script)
		}
	}
	if script != applyScript {
		t.Fatalf("unexpected script %q", script)
	}
}

func TestNonZeroExitCodeIsError(t *testing.T) {
	vm, a := newFakeVM(t)
	vm.files["/home/agentos/a.txt"] = []byte("hi")
	vm.failRun = true
	err := a.Load(context.Background(), newWorld())
	if err == nil || !strings.Contains(err.Error(), "run failed") {
		t.Fatalf("expected run failure, got %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("error leaked token")
	}
}

func TestNon2xxIsErrorWithoutToken(t *testing.T) {
	vm, a := newFakeVM(t)
	vm.failHTTP = true
	err := a.Load(context.Background(), newWorld())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token: %v", err)
	}
	err = a.Apply(context.Background(), func() *vfs.World {
		w := newWorld()
		_ = w.WriteFile("a.txt", []byte("x"), 0o644)
		return w
	}())
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unexpected apply error %v", err)
	}
}

func TestLoadExcludesSizeAndSymlinks(t *testing.T) {
	vm, a := newFakeVM(t)
	vm.files["/home/agentos/good.txt"] = []byte("good")
	vm.files["/home/agentos/.git/config"] = []byte("secret")
	vm.files["/home/agentos/node_modules/x/index.js"] = []byte("js")
	vm.files["/home/agentos/big.bin"] = make([]byte, 1024)
	vm.symlinks["/home/agentos/link"] = true
	a.MaxFileSize = 500

	w := newWorld()
	if err := a.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadFile("good.txt"); err != nil {
		t.Fatal("expected good.txt")
	}
	for _, p := range []string{".git/config", "node_modules/x/index.js", "big.bin", "link"} {
		if _, err := w.ReadFile(p); err == nil {
			t.Fatalf("expected %s to be skipped", p)
		}
	}
	sort.Strings(a.ExcludedPaths)
	want := []string{"/home/agentos/big.bin", "/home/agentos/link"}
	if strings.Join(a.ExcludedPaths, ",") != strings.Join(want, ",") {
		t.Fatalf("ExcludedPaths = %v, want %v", a.ExcludedPaths, want)
	}
	for _, r := range vm.requests {
		if strings.HasSuffix(r.path, "v1.process.run") && (bytes.Contains(r.body, []byte("big.bin")) || bytes.Contains(r.body, []byte("link"))) {
			t.Fatalf("skipped file was read: %s", r.body)
		}
	}
}

func TestResolveTargetRefusesEscape(t *testing.T) {
	for _, rel := range []string{"../escape.txt", "a/../../escape.txt", "../../etc/passwd", "x" + string(rune(0)) + "y/../.."} {
		if _, err := resolveTarget("/home/agentos", rel); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("resolveTarget(%q) err = %v, want ErrPathEscape", rel, err)
		}
	}
	got, err := resolveTarget("/home/agentos", "a/b.txt")
	if err != nil || got != "/home/agentos/a/b.txt" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = resolveTarget("/", "a.txt")
	if err != nil || got != "/a.txt" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNewRequiresToken(t *testing.T) {
	t.Setenv("AGENTOS_VM_TOKEN", "")
	if _, err := New(context.Background(), "vm-1", ""); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestNewRequiresVMID(t *testing.T) {
	t.Setenv("AGENTOS_VM_TOKEN", testToken)
	if _, err := New(context.Background(), "", ""); err == nil {
		t.Fatal("expected error for missing vm id")
	}
}

func TestNewDefaults(t *testing.T) {
	t.Setenv("AGENTOS_VM_TOKEN", testToken)
	t.Setenv("AGENTOS_ENDPOINT", "https://cluster-api.example.dev/")
	a, err := New(context.Background(), "vm-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Root != "/home/agentos" {
		t.Fatalf("Root = %q", a.Root)
	}
	if a.endpoint != "https://cluster-api.example.dev" {
		t.Fatalf("endpoint = %q", a.endpoint)
	}
	if a.MaxFileSize != 10*1024*1024 || len(a.ExcludeGlobs) != 5 {
		t.Fatalf("unexpected defaults: %d %v", a.MaxFileSize, a.ExcludeGlobs)
	}

	t.Setenv("AGENTOS_ENDPOINT", "")
	a, err = New(context.Background(), "vm-1", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if a.endpoint != "https://api.rivet.dev" || a.Root != "/x" {
		t.Fatalf("unexpected: %q %q", a.endpoint, a.Root)
	}
}

func TestTransportErrorDoesNotLeakToken(t *testing.T) {
	t.Setenv("AGENTOS_VM_TOKEN", testToken)
	a, err := New(context.Background(), "vm-1", "", WithEndpoint("http://127.0.0.1:1/"+testToken))
	if err != nil {
		t.Fatal(err)
	}
	err = a.Load(context.Background(), newWorld())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token: %v", err)
	}
}

func TestFidelityTierB(t *testing.T) {
	_, a := newFakeVM(t)
	f := a.Fidelity()
	if f.Tier != source.TierB {
		t.Fatalf("tier = %s", f.Tier)
	}
	if len(f.Limitations) == 0 {
		t.Fatal("expected limitations")
	}
}

func TestIntegration(t *testing.T) {
	endpoint := os.Getenv("AGENTOS_ENDPOINT")
	vmID := os.Getenv("AGENTOS_VM_ID")
	if endpoint == "" || vmID == "" || os.Getenv("AGENTOS_VM_TOKEN") == "" {
		t.Skip("AGENTOS_ENDPOINT, AGENTOS_VM_ID and AGENTOS_VM_TOKEN are required")
	}
	ctx := context.Background()

	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	root := "/tmp/zygote-it-" + hex.EncodeToString(suffix[:])

	a, err := New(ctx, vmID, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.run(context.Background(), "rm", "-rf", root) })

	w := newWorld()
	if err := w.WriteFile("nested/data.bin", binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, w); err != nil {
		t.Fatal(err)
	}

	loaded := newWorld()
	if err := a.Load(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	got, err := loaded.ReadFile("nested/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("mismatch: %v", got)
	}
}
