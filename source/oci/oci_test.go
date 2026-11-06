package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/vfs"
)

type tarEntry struct {
	name string
	body string
	mode int64
	typ  byte
	link string
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: typ, Linkname: e.link}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type recorded struct {
	mu       sync.Mutex
	requests []string
	queries  []string
}

func (r *recorded) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	r.queries = append(r.queries, req.URL.Query().Get("path"))
}

func newAdapter(t *testing.T, h http.Handler) *Adapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	a, err := New(context.Background(), "c1", "/work", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func okInspect(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/containers/c1/json" {
		_, _ = w.Write([]byte(`{"Id":"c1"}`))
		return true
	}
	return false
}

func TestNew_InspectsContainer(t *testing.T) {
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		okInspect(w, r)
	}))
	defer srv.Close()

	if _, err := New(context.Background(), "c1", "/work", WithBaseURL(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 || rec.requests[0] != "GET /containers/c1/json" {
		t.Fatalf("unexpected requests: %v", rec.requests)
	}
}

func TestNew_ContainerNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: ghost"}`))
	}))
	defer srv.Close()

	_, err := New(context.Background(), "ghost", "/work", WithBaseURL(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `container "ghost" not found`) || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestNew_RequiresArguments(t *testing.T) {
	if _, err := New(context.Background(), "", "/work"); err == nil {
		t.Fatal("expected error for empty container id")
	}
	if _, err := New(context.Background(), "c1", ""); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestNew_DockerHostTCP(t *testing.T) {
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		okInspect(w, r)
	}))
	defer srv.Close()

	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	if _, err := New(context.Background(), "c1", "/work"); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 {
		t.Fatalf("unexpected requests: %v", rec.requests)
	}
}

func TestNew_DockerHostUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	rec := &recorded{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		okInspect(w, r)
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	t.Setenv("DOCKER_HOST", "unix://"+filepath.ToSlash(sock))
	if _, err := New(context.Background(), "c1", "/work"); err != nil {
		t.Fatal(err)
	}
	if len(rec.requests) != 1 || rec.requests[0] != "GET /containers/c1/json" {
		t.Fatalf("unexpected requests: %v", rec.requests)
	}
}

func TestResolveHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	if got := resolveHost(); got != "unix:///var/run/docker.sock" {
		t.Fatalf("default host = %q", got)
	}
	t.Setenv("DOCKER_HOST", "tcp://h:1")
	if got := resolveHost(); got != "tcp://h:1" {
		t.Fatalf("host = %q", got)
	}
}

func TestConnect(t *testing.T) {
	_, base, err := connect("unix:///var/run/docker.sock")
	if err != nil || base != "http://docker" {
		t.Fatalf("unix: base=%q err=%v", base, err)
	}
	_, base, err = connect("tcp://10.0.0.1:2375")
	if err != nil || base != "http://10.0.0.1:2375" {
		t.Fatalf("tcp: base=%q err=%v", base, err)
	}
	for _, bad := range []string{"ftp://x", "unix://", "tcp://", "://"} {
		if _, _, err := connect(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestLoad(t *testing.T) {
	archive := buildTar(t, []tarEntry{
		{name: "work/", typ: tar.TypeDir, mode: 0o755},
		{name: "work/a.txt", body: "hello", mode: 0o644},
		{name: "work/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "work/bin/run.sh", body: "#!/bin/sh", mode: 0o755},
		{name: "work/.git/config", body: "secret", mode: 0o644},
		{name: "work/node_modules/x/y.js", body: "js", mode: 0o644},
		{name: "work/big.bin", body: strings.Repeat("x", 100), mode: 0o644},
		{name: "work/link", typ: tar.TypeSymlink, link: "a.txt", mode: 0o777},
		{name: "work/hard", typ: tar.TypeLink, link: "work/a.txt", mode: 0o644},
	})

	rec := &recorded{}
	a := newAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if okInspect(w, r) {
			return
		}
		_, _ = w.Write(archive)
	}))
	a.MaxFileSize = 50

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := a.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	last := len(rec.requests) - 1
	if rec.requests[last] != "GET /containers/c1/archive" || rec.queries[last] != "/work" {
		t.Fatalf("unexpected request: %v %v", rec.requests, rec.queries)
	}

	got, err := w.ReadFile("a.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
	info, err := w.Stat("bin/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	info, err = w.Stat("a.txt")
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("a.txt mode = %v, %v", info, err)
	}

	for _, missing := range []string{".git/config", "node_modules/x/y.js", "big.bin", "link", "hard"} {
		if _, err := w.ReadFile(missing); err == nil {
			t.Fatalf("%s should not be loaded", missing)
		}
	}

	want := map[string]bool{"big.bin": true, "link": true, "hard": true}
	if len(a.ExcludedPaths) != len(want) {
		t.Fatalf("ExcludedPaths = %v", a.ExcludedPaths)
	}
	for _, p := range a.ExcludedPaths {
		if !want[p] {
			t.Fatalf("unexpected excluded path %q", p)
		}
	}
}

func TestLoad_PathEscape(t *testing.T) {
	for _, name := range []string{"work/../../etc/passwd", "/etc/passwd", "../x"} {
		archive := buildTar(t, []tarEntry{{name: name, body: "x", mode: 0o644}})
		a := newAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if okInspect(w, r) {
				return
			}
			_, _ = w.Write(archive)
		}))
		err := a.Load(context.Background(), vfs.NewWorld(vfs.NewMemStore()))
		if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%q: err = %v", name, err)
		}
	}
}

func TestApply(t *testing.T) {
	var (
		mu          sync.Mutex
		method      string
		urlPath     string
		query       string
		contentType string
		body        []byte
	)
	a := newAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if okInspect(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		method, urlPath, query, contentType, body = r.Method, r.URL.Path, r.URL.Query().Get("path"), r.Header.Get("Content-Type"), b
		mu.Unlock()
	}))

	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}
	w := vfs.NewWorld(vfs.NewMemStore())
	if err := w.WriteFile("a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("sub/run.sh", []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("sub/deep/data.bin", binary, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPut || urlPath != "/containers/c1/archive" || query != "/work" {
		t.Fatalf("request = %s %s ?path=%s", method, urlPath, query)
	}
	if contentType != "application/x-tar" {
		t.Fatalf("Content-Type = %q", contentType)
	}

	type got struct {
		mode int64
		typ  byte
		body []byte
	}
	entries := map[string]got{}
	var order []string
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		entries[hdr.Name] = got{hdr.Mode, hdr.Typeflag, data}
		order = append(order, hdr.Name)
	}

	checks := []struct {
		name string
		mode int64
		typ  byte
		body []byte
	}{
		{"a.txt", 0o644, tar.TypeReg, []byte("hello")},
		{"sub/run.sh", 0o755, tar.TypeReg, []byte("#!/bin/sh")},
		{"sub/deep/data.bin", 0o600, tar.TypeReg, binary},
	}
	if len(entries) != len(checks) {
		t.Fatalf("entries = %v", order)
	}
	for _, c := range checks {
		e, ok := entries[c.name]
		if !ok {
			t.Fatalf("missing %s in %v", c.name, order)
		}
		if e.mode != c.mode || e.typ != c.typ || !bytes.Equal(e.body, c.body) {
			t.Fatalf("%s = mode %o typ %c len %d", c.name, e.mode, e.typ, len(e.body))
		}
	}
	for name, e := range entries {
		if e.typ != tar.TypeReg {
			t.Fatalf("%s: non-regular entry type %c", name, e.typ)
		}
		if name == "." || name == "./" || strings.HasSuffix(name, "/") {
			t.Fatalf("unexpected directory or root entry %q", name)
		}
	}
}

func TestCheckPath(t *testing.T) {
	for _, bad := range []string{"", "/abs", "../x", "a/../../x", "a/../b", "./a"} {
		if err := checkPath(bad); !errors.Is(err, ErrPathEscape) {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
	for _, good := range []string{"a", "a/b.txt", "..hidden/x"} {
		if err := checkPath(good); err != nil {
			t.Fatalf("%q: err = %v", good, err)
		}
	}
}

func TestEngineErrors(t *testing.T) {
	a := newAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if okInspect(w, r) {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom: no space left"}`))
	}))

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := w.WriteFile("a.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Load(context.Background(), vfs.NewWorld(vfs.NewMemStore())); err == nil || !strings.Contains(err.Error(), "boom: no space left") {
		t.Fatalf("Load err = %v", err)
	}
	if err := a.Apply(context.Background(), w); err == nil || !strings.Contains(err.Error(), "boom: no space left") {
		t.Fatalf("Apply err = %v", err)
	}
}

func TestEngineError_PlainBodyIsCapped(t *testing.T) {
	a := newAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if okInspect(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("z", 100000)))
	}))
	err := a.Load(context.Background(), vfs.NewWorld(vfs.NewMemStore()))
	if err == nil || len(err.Error()) > 5000 || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestFidelity(t *testing.T) {
	a := &Adapter{MaxFileSize: 10, ExcludeGlobs: []string{".git"}}
	f := a.Fidelity()
	if f.Tier != source.TierB {
		t.Fatalf("tier = %s", f.Tier)
	}
	joined := strings.Join(f.Limitations, "\n")
	for _, want := range []string{"step boundaries", "symlinks", "never deletes", "uid/gid", "FR-7.3", "FR-7.2", "memory"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("limitations missing %q: %v", want, f.Limitations)
		}
	}
}

func execInContainer(t *testing.T, a *Adapter, cmd ...string) {
	t.Helper()
	ctx := context.Background()
	create := `{"Cmd":[` + quoteAll(cmd) + `]}`
	resp, err := a.request(ctx, http.MethodPost, a.containerEndpoint("exec"), nil, strings.NewReader(create), "application/json")
	if err != nil {
		t.Logf("cleanup exec create: %v", err)
		return
	}
	defer closeBody(resp.Body)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	id := between(string(raw), `"Id":"`, `"`)
	if id == "" {
		return
	}
	start, err := a.request(ctx, http.MethodPost, "/exec/"+id+"/start", nil, strings.NewReader(`{"Detach":true}`), "application/json")
	if err != nil {
		t.Logf("cleanup exec start: %v", err)
		return
	}
	_ = start.Body.Close()
}

func quoteAll(ss []string) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = `"` + s + `"`
	}
	return strings.Join(parts, ",")
}

func between(s, left, right string) string {
	_, rest, ok := strings.Cut(s, left)
	if !ok {
		return ""
	}
	val, _, _ := strings.Cut(rest, right)
	return val
}

func TestIntegration_RoundTrip(t *testing.T) {
	id := os.Getenv("OCI_CONTAINER_ID")
	if id == "" {
		t.Skip("OCI_CONTAINER_ID not set")
	}
	ctx := context.Background()

	dir := "zygote-it-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + filepath.Base(t.TempDir())
	dir = strings.ToLower(strings.NewReplacer("_", "-", " ", "-").Replace(dir))

	parent, err := New(ctx, id, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { execInContainer(t, parent, "rm", "-rf", "/tmp/"+dir) })

	binary := make([]byte, 1024)
	for i := range binary {
		binary[i] = byte(i % 256)
	}
	src := vfs.NewWorld(vfs.NewMemStore())
	files := map[string]struct {
		data []byte
		mode uint32
	}{
		dir + "/a.txt":          {[]byte("hello"), 0o644},
		dir + "/sub/run.sh":     {[]byte("#!/bin/sh\n"), 0o755},
		dir + "/sub/blob.bin":   {binary, 0o600},
		dir + "/sub/empty.file": {nil, 0o644},
	}
	for p, f := range files {
		if err := src.WriteFile(p, f.data, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := parent.Apply(ctx, src); err != nil {
		t.Fatal(err)
	}

	a, err := New(ctx, id, "/tmp/"+dir)
	if err != nil {
		t.Fatal(err)
	}
	dst := vfs.NewWorld(vfs.NewMemStore())
	if err := a.Load(ctx, dst); err != nil {
		t.Fatal(err)
	}

	for p, f := range files {
		rel := strings.TrimPrefix(p, dir+"/")
		got, err := dst.ReadFile(rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !bytes.Equal(got, f.data) {
			t.Fatalf("%s: content mismatch", rel)
		}
		info, err := dst.Stat(rel)
		if err != nil {
			t.Fatal(err)
		}
		if uint32(info.Mode().Perm()) != f.mode {
			t.Fatalf("%s: mode %o, want %o", rel, info.Mode().Perm(), f.mode)
		}
	}
}
