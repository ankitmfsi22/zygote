package mcp

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

func rewriteBundle(t *testing.T, src, dst string, keep func(name string) bool) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck

	f, err := os.Create(dst) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, zf := range zr.File {
		if !keep(zf.Name) {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create(zf.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunEffectChunksExportVerify(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "hello world", Chunks: []int{5, 1, 5}})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "http", Key: "u", Value: "plain"})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	b := importBundle(t, filepath.Join(root, "run.zip"))
	if got := b.Recording.Effects[0].Chunks; !reflect.DeepEqual(got, []int{5, 1, 5}) {
		t.Fatalf("chunks not exported: %v", got)
	}
	if string(b.Recording.Effects[0].Value) != "hello world" || b.Recording.Effects[1].Chunks != nil {
		t.Fatalf("unexpected effects %+v", b.Recording.Effects)
	}

	out := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip"})
	if !out.Valid || out.Status != statusMatch {
		t.Fatalf("unexpected verify result %+v", out)
	}
}

func TestRunEffectInvalidChunksAreToolErrorsAndDoNotPoison(t *testing.T) {
	cs := newSession(t, t.TempDir(), 0)
	call[RunStartOutput](t, cs, "run.start", RunStartInput{})

	for _, chunks := range [][]int{{1, 1}, {0, 5}, {-1, 6}, {9}} {
		callErr(t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "abcde", Chunks: chunks}, "chunk")
	}
	callErr(t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "!!", Encoding: "base64", Chunks: []int{1}}, "base64")

	e := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "abcde", Chunks: []int{2, 3}})
	if e.Seq != 0 {
		t.Fatalf("rejected effects must not be recorded or poison the run: %+v", e)
	}
}

func TestRunEffectChunksAreBytesNotCharacters(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	call[RunStartOutput](t, cs, "run.start", RunStartInput{})

	callErr(t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "a€", Chunks: []int{1, 1}}, "chunk")
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "p", Value: "a€", Chunks: []int{1, 3}})
}

func TestRunEffectStreamedToolAutoStep(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{AutoStep: true})
	e := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "tool:search", Key: "q", Value: "result", Chunks: []int{3, 3}})
	if e.AutoStep == nil || e.AutoStep.Name != "auto:tool:search" || e.AutoStep.Effects != 1 {
		t.Fatalf("a streamed tool effect must close an auto step: %+v", e)
	}
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	b := importBundle(t, filepath.Join(root, "run.zip"))
	if !reflect.DeepEqual(b.Recording.Effects[0].Chunks, []int{3, 3}) {
		t.Fatalf("unexpected effects %+v", b.Recording.Effects)
	}
}

func TestLargeValueOverMCPIsSpilledAndReplays(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	large := strings.Repeat("0123456789", (trace.DefaultSpillThreshold/10)+10)
	half := len(large) / 2

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "big", Value: large, Chunks: []int{half, len(large) - half}})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "http", Key: "small", Value: "tiny"})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	zr, err := zip.OpenReader(filepath.Join(root, "run.zip"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []byte
	for _, f := range zr.File {
		if f.Name == "trace.json" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ = io.ReadAll(rc)
			_ = rc.Close()
		}
	}
	_ = zr.Close()
	if bytes.Contains(manifest, []byte(large)) || !bytes.Contains(manifest, []byte(`"value_ref"`)) || !bytes.Contains(manifest, []byte(`"value_size"`)) {
		t.Fatalf("large value must be stored by reference: %.400s", manifest)
	}

	store := vfs.NewMemStore()
	bundle, err := trace.ImportFile(filepath.Join(root, "run.zip"), store)
	if err != nil {
		t.Fatal(err)
	}
	big, small := bundle.Recording.Effects[0], bundle.Recording.Effects[1]
	if big.ValueRef == "" || len(big.Value) != 0 || big.ValueSize != len(large) || small.ValueRef != "" || string(small.Value) != "tiny" {
		t.Fatalf("unexpected entries %+v %+v", big.ValueRef, small)
	}
	rr, err := trace.Replay(store, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	val, chunks, err := rr.ReplayEffectChunks("model", "big")
	if err != nil || string(val) != large || !reflect.DeepEqual(chunks, []int{half, len(large) - half}) {
		t.Fatalf("replay of the spilled value failed: %v %v", chunks, err)
	}

	out := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip"})
	if !out.Valid || out.Status != statusMatch {
		t.Fatalf("unexpected verify result %+v", out)
	}
}

func TestReplayVerifyRejectsBundleMissingEffectObject(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	large := strings.Repeat("z", trace.DefaultSpillThreshold+1)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "big", Value: large})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	b := importBundle(t, filepath.Join(root, "run.zip"))
	drop := "objects/" + b.Recording.Effects[0].ValueRef
	rewriteBundle(t, filepath.Join(root, "run.zip"), filepath.Join(root, "broken.zip"), func(name string) bool { return name != drop })

	out := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "broken.zip"})
	if out.Valid || out.Status != statusInvalid {
		t.Fatalf("a bundle missing an effect object must be invalid: %+v", out)
	}
}

func TestLargeSecretOverMCPIsBlockedWithoutPolicy(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	large := strings.Repeat("x", trace.DefaultSpillThreshold+1) + awsKey

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "big", Value: large})
	callErr(t, cs, "run.export", RunExportInput{Out: "run.zip"}, "fail-closed")
	if _, err := os.Stat(filepath.Join(root, "run.zip")); err == nil {
		t.Fatal("a blocked export must not write a bundle")
	}
}

func TestLargeSecretOverMCPIsRedactedBeforeSpill(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".zygote", "redact.yaml"), "rules:\n  - id: aws-access-token\n    action: redact\n")
	cs := newSession(t, root, 0)
	large := strings.Repeat("x", trace.DefaultSpillThreshold+1) + awsKey

	call[RunStartOutput](t, cs, "run.start", RunStartInput{Dir: "."})
	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "big", Value: large})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})

	zr, err := zip.OpenReader(filepath.Join(root, "run.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck
	objects := 0
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if bytes.Contains(data, []byte(awsKey)) {
			t.Fatalf("secret leaked into %s", f.Name)
		}
		if strings.HasPrefix(f.Name, "objects/") && bytes.Contains(data, []byte("***")) {
			objects++
		}
	}
	if objects != 1 {
		t.Fatalf("expected exactly one redacted effect object, found %d", objects)
	}
}
