package daytona_test

import (
	"context"
	"errors"
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/daytona/clients/sdk-go/pkg/types"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/source/daytona"
	"github.com/mindfire/zygote/vfs"
)

type fakeFS struct {
	files   map[string][]byte
	folders []string
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}}
}

func (f *fakeFS) ListFiles(_ context.Context, dir string) ([]*types.FileInfo, error) {
	seen := map[string]*types.FileInfo{}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for p, data := range f.files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		name, _, nested := strings.Cut(rest, "/")
		if nested {
			seen[name] = &types.FileInfo{Name: name, Path: prefix + name, IsDirectory: true}
			continue
		}
		seen[name] = &types.FileInfo{Name: name, Path: p, Size: int64(len(data)), Mode: "0644"}
	}
	out := make([]*types.FileInfo, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeFS) DownloadFile(_ context.Context, p string) ([]byte, error) {
	data, ok := f.files[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

func (f *fakeFS) UploadFile(_ context.Context, data []byte, p string) error {
	f.files[p] = data
	return nil
}

func (f *fakeFS) CreateFolder(_ context.Context, dir string) error {
	f.folders = append(f.folders, dir)
	return nil
}

func TestRoundTrip(t *testing.T) {
	src := newFakeFS()
	src.files["/work/a.txt"] = []byte("a")
	src.files["/work/sub/b.txt"] = []byte("b")

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := daytona.NewWithFileSystem(src, "/work").Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	got, err := w.ReadFile("sub/b.txt")
	if err != nil || string(got) != "b" {
		t.Fatalf("unexpected sub/b.txt: %q, %v", got, err)
	}

	dst := newFakeFS()
	dst.files["/work/unrelated.txt"] = []byte("keep")
	if err := daytona.NewWithFileSystem(dst, "/work").Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	if string(dst.files["/work/a.txt"]) != "a" || string(dst.files["/work/sub/b.txt"]) != "b" {
		t.Fatalf("files not applied: %v", dst.files)
	}
	if string(dst.files["/work/unrelated.txt"]) != "keep" {
		t.Fatal("unrelated file must be preserved")
	}
	if len(dst.folders) == 0 {
		t.Fatal("expected folders to be created")
	}
}

func TestLoad_Exclusions(t *testing.T) {
	src := newFakeFS()
	src.files["/work/good.txt"] = []byte("hello")
	src.files["/work/.git/config"] = []byte("secret")
	src.files["/work/big.txt"] = make([]byte, 1024)

	a := daytona.NewWithFileSystem(src, "/work")
	a.MaxFileSize = 500

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := a.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	if _, err := w.ReadFile("good.txt"); err != nil {
		t.Fatal("expected good.txt to be loaded")
	}
	if _, err := w.ReadFile(".git/config"); err == nil {
		t.Fatal("expected .git/config to be skipped")
	}
	if _, err := w.ReadFile("big.txt"); err == nil {
		t.Fatal("expected big.txt to be skipped")
	}
	if len(a.ExcludedPaths) != 1 || a.ExcludedPaths[0] != "/work/big.txt" {
		t.Fatalf("unexpected excluded paths: %v", a.ExcludedPaths)
	}
}

func TestContextCancelled(t *testing.T) {
	src := newFakeFS()
	src.files["/work/a.txt"] = []byte("a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := daytona.NewWithFileSystem(src, "/work").Load(ctx, w); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFidelity(t *testing.T) {
	f := daytona.NewWithFileSystem(newFakeFS(), "/work").Fidelity()
	if f.Tier != source.TierB {
		t.Fatalf("expected Tier B, got %s", f.Tier)
	}
	if len(f.Limitations) == 0 {
		t.Fatal("expected limitations")
	}
}

func TestIntegration(t *testing.T) {
	if os.Getenv("DAYTONA_API_KEY") == "" || os.Getenv("DAYTONA_SANDBOX_ID") == "" {
		t.Skip("DAYTONA_API_KEY and DAYTONA_SANDBOX_ID not set")
	}

	ctx := context.Background()
	root := path.Join("/tmp", "zygote-integration")

	a, err := daytona.New(ctx, os.Getenv("DAYTONA_SANDBOX_ID"), root)
	if err != nil {
		t.Fatal(err)
	}

	w := vfs.NewWorld(vfs.NewMemStore())
	if err := w.WriteFile("hello/world.txt", []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, w); err != nil {
		t.Fatal(err)
	}

	w2 := vfs.NewWorld(vfs.NewMemStore())
	if err := a.Load(ctx, w2); err != nil {
		t.Fatal(err)
	}
	got, err := w2.ReadFile("hello/world.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("round trip mismatch: %q, %v", got, err)
	}
}
