package trace_test

import (
	"archive/zip"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

const (
	benchObjects       = 100_000
	benchFilesPerDir   = 99
	benchImportBudget  = 5 * time.Second
	benchObjectPadding = 192
)

func buildBenchBundle(tb testing.TB) string {
	tb.Helper()

	files := benchObjects - 1
	dirs := 0
	for rem := files; rem > 0; dirs++ {
		rem -= benchFilesPerDir + 1
	}
	files -= dirs

	store := vfs.NewMemStore()
	padding := strings.Repeat("x", benchObjectPadding)
	root := make(vfs.Tree, 0, dirs)
	for d, i := 0, 0; i < files; d++ {
		dir := make(vfs.Tree, 0, benchFilesPerDir)
		for ; i < files && len(dir) < benchFilesPerDir; i++ {
			data := fmt.Appendf(nil, "zygote bench object %d\n%s", i, padding)
			h, err := store.Put(data)
			if err != nil {
				tb.Fatal(err)
			}
			dir = append(dir, vfs.Entry{Name: fmt.Sprintf("f%05d.txt", len(dir)), Kind: vfs.KindFile, Hash: h, Mode: 0o644, Size: int64(len(data))})
		}
		dir.Sort()
		dh, err := store.Put(dir.Encode())
		if err != nil {
			tb.Fatal(err)
		}
		root = append(root, vfs.Entry{Name: fmt.Sprintf("d%04d", d), Kind: vfs.KindDir, Hash: dh, Mode: 0o755})
	}
	root.Sort()
	rh, err := store.Put(root.Encode())
	if err != nil {
		tb.Fatal(err)
	}

	rec := trace.Recording{
		Version:       1,
		HashAlgorithm: "blake3",
		Producer:      "zygote-bench",
		Steps:         []trace.Step{{N: 0, Name: "init", Root: hex.EncodeToString(rh[:])}},
	}
	path := filepath.Join(tb.TempDir(), "bench.zip")
	if err := trace.ExportFile(path, store, rec); err != nil {
		tb.Fatal(err)
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck
	count := 0
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "objects/") {
			count++
		}
	}
	if count != benchObjects {
		tb.Fatalf("fixture has %d objects, want exactly %d", count, benchObjects)
	}
	return path
}

func BenchmarkImportBundle100k(b *testing.B) {
	path := buildBenchBundle(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		bundle, err := trace.ImportFile(path, vfs.NewMemStore())
		elapsed := time.Since(start)
		if err != nil {
			b.Fatal(err)
		}
		if len(bundle.Recording.Steps) != 1 {
			b.Fatalf("expected 1 step, got %d", len(bundle.Recording.Steps))
		}
		if elapsed > benchImportBudget {
			b.Fatalf("importing %d objects took %s, budget %s (NFR-1.7)", benchObjects, elapsed, benchImportBudget)
		}
	}
}
