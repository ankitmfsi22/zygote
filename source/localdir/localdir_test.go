package localdir_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mindfire/zygote/source"
	"github.com/mindfire/zygote/source/localdir"
	"github.com/mindfire/zygote/vfs"
)

func TestLoad_Exclusions(t *testing.T) {
	dir := t.TempDir()

	_ = os.WriteFile(filepath.Join(dir, "good.txt"), []byte("hello"), 0644) //nolint:gosec

	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0755)                              //nolint:gosec
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("secret"), 0644) //nolint:gosec

	_ = os.WriteFile(filepath.Join(dir, "big.txt"), make([]byte, 1024), 0644) //nolint:gosec

	adapter := localdir.New(dir)
	adapter.MaxFileSize = 500 // skip big.txt

	store := vfs.NewMemStore()
	w := vfs.NewWorld(store)

	err := adapter.Load(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.ReadFile("good.txt")
	if err != nil {
		t.Fatal("expected good.txt to be loaded")
	}

	_, err = w.ReadFile(".git/config")
	if err == nil {
		t.Fatal("expected .git/config to be skipped")
	}

	_, err = w.ReadFile("big.txt")
	if err == nil {
		t.Fatal("expected big.txt to be skipped")
	}

	if len(adapter.ExcludedPaths) != 1 {
		t.Fatalf("expected 1 excluded path, got %d", len(adapter.ExcludedPaths))
	}
}

func TestApply_Security(t *testing.T) {
	dir := t.TempDir()
	adapter := localdir.New(dir)

	store := vfs.NewMemStore()
	w := vfs.NewWorld(store)

	// Inject a malicious path into the VFS directly
	err := w.WriteFile("../escaped.txt", []byte("bad"), 0644)
	_ = err

	_ = w.WriteFile("normal.txt", []byte("ok"), 0644)
	err = adapter.Apply(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	_ = os.WriteFile(filepath.Join(srcDir, "test.go"), []byte("package main"), 0644) //nolint:gosec

	store := vfs.NewMemStore()
	w := vfs.NewWorld(store)

	srcAdapter := localdir.New(srcDir)
	if err := srcAdapter.Load(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	// Modify VFS
	_ = w.WriteFile("test.go", []byte("package main\n\nfunc main() {}"), 0644)
	_ = w.WriteFile("new.txt", []byte("new"), 0644)

	dstAdapter := localdir.New(dstDir)
	if err := dstAdapter.Apply(context.Background(), w); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(filepath.Join(dstDir, "test.go")) //nolint:gosec
	if string(b) != "package main\n\nfunc main() {}" {
		t.Fatal("Apply did not write correct contents")
	}

	b, _ = os.ReadFile(filepath.Join(dstDir, "new.txt")) //nolint:gosec
	if string(b) != "new" {
		t.Fatal("Apply did not write new file")
	}
}

var _ source.Source = (*localdir.Adapter)(nil)

func TestFidelity(t *testing.T) {
	a := localdir.New(t.TempDir())
	a.MaxFileSize = 1234
	a.ExcludeGlobs = []string{"*.secret", "vendor"}

	f := a.Fidelity()
	if f.Tier != source.TierA {
		t.Fatalf("expected Tier A, got %q", f.Tier)
	}

	joined := strings.Join(f.Limitations, "\n")
	for _, want := range []string{"symlinks", "1234 bytes", "*.secret", "vendor"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limitations missing %q: %v", want, f.Limitations)
		}
	}
}

func TestFidelity_NoExcludeGlobs(t *testing.T) {
	a := localdir.New(t.TempDir())
	a.ExcludeGlobs = nil

	for _, l := range a.Fidelity().Limitations {
		if strings.Contains(l, "FR-7.2") {
			t.Fatalf("unexpected glob limitation: %q", l)
		}
	}
}
