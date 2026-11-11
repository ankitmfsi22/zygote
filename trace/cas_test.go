package trace_test

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

type effectSpec struct {
	op     string
	key    string
	value  []byte
	chunks []int
}

func recordRun(t *testing.T, specs ...effectSpec) (trace.Recording, vfs.Store) {
	t.Helper()
	store := vfs.NewMemStore()
	r := trace.Record(store)
	if err := r.World().WriteFile("a.txt", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Step("init")
	for _, s := range specs {
		if _, _, err := r.RecordEffectChunks(s.op, s.key, func() ([]byte, []int, error) { return s.value, s.chunks, nil }); err != nil {
			t.Fatal(err)
		}
	}
	r.Step("end")
	return r.Recording(), store
}

func exportTo(t *testing.T, name string, rec trace.Recording, store vfs.Store, opts ...trace.ExportOption) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := trace.ExportFile(path, store, rec, opts...); err != nil {
		t.Fatal(err)
	}
	return path
}

func readZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck
	files := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = b
	}
	return files
}

func writeZip(t *testing.T, files map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tampered.zip")
	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func manifestOf(t *testing.T, files map[string][]byte) trace.Recording {
	t.Helper()
	var rec trace.Recording
	if err := json.Unmarshal(files["trace.json"], &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func editManifest(t *testing.T, files map[string][]byte, edit func(*trace.Recording)) {
	t.Helper()
	rec := manifestOf(t, files)
	edit(&rec)
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	files["trace.json"] = b
}

func objectNames(files map[string][]byte) []string {
	var names []string
	for n := range files {
		if strings.HasPrefix(n, "objects/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func hashOf(t *testing.T, b []byte) string {
	t.Helper()
	h, err := vfs.NewMemStore().Put(b)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h[:])
}

func filler(n int, c byte) []byte {
	return bytes.Repeat([]byte{c}, n)
}

func importFrom(t *testing.T, path string) trace.Bundle {
	t.Helper()
	b, err := trace.ImportFile(path, vfs.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSpillBoundary(t *testing.T) {
	small := filler(10, 's')
	large := filler(11, 'l')
	rec, store := recordRun(t,
		effectSpec{"model", "a", small, nil},
		effectSpec{"model", "b", large, []int{5, 6}},
	)

	files := readZip(t, exportTo(t, "run.zip", rec, store, trace.WithSpillThreshold(10)))
	m := manifestOf(t, files)

	if !bytes.Equal(m.Effects[0].Value, small) || m.Effects[0].ValueRef != "" || m.Effects[0].ValueSize != 0 {
		t.Fatalf("value at the threshold must stay inline: %+v", m.Effects[0])
	}
	e := m.Effects[1]
	if len(e.Value) != 0 || e.ValueRef != hashOf(t, large) || e.ValueSize != len(large) || !reflect.DeepEqual(e.Chunks, []int{5, 6}) {
		t.Fatalf("value above the threshold must be a reference: %+v", e)
	}
	if !bytes.Equal(files["objects/"+e.ValueRef], large) {
		t.Fatal("referenced object missing or wrong in bundle")
	}
	if bytes.Contains(files["trace.json"], []byte("bGxsbGxs")) || bytes.Contains(files["trace.json"], large) {
		t.Fatalf("manifest still carries the large value: %s", files["trace.json"])
	}
}

func TestDefaultSpillThreshold(t *testing.T) {
	atLimit := filler(trace.DefaultSpillThreshold, 'a')
	over := filler(trace.DefaultSpillThreshold+1, 'b')
	rec, store := recordRun(t,
		effectSpec{"http", "a", atLimit, nil},
		effectSpec{"http", "b", over, nil},
	)

	if trace.DefaultSpillThreshold != 32<<10 {
		t.Fatalf("default threshold changed: %d", trace.DefaultSpillThreshold)
	}
	m := manifestOf(t, readZip(t, exportTo(t, "run.zip", rec, store)))
	if m.Effects[0].ValueRef != "" || m.Effects[1].ValueRef == "" {
		t.Fatalf("default threshold misapplied: %+v %+v", m.Effects[0].ValueRef, m.Effects[1].ValueRef)
	}

	m = manifestOf(t, readZip(t, exportTo(t, "never.zip", rec, store, trace.WithSpillThreshold(-1))))
	if m.Effects[0].ValueRef != "" || m.Effects[1].ValueRef != "" {
		t.Fatal("negative threshold must never spill")
	}

	m = manifestOf(t, readZip(t, exportTo(t, "zero.zip", rec, store, trace.WithSpillThreshold(0))))
	if m.Effects[0].ValueRef != "" || m.Effects[1].ValueRef == "" {
		t.Fatal("zero threshold must select the default")
	}
}

func TestSpillRoundTripReplay(t *testing.T) {
	small := []byte("tiny")
	large := bytes.Repeat([]byte("stream "), 30)
	chunks := []int{7, 7, len(large) - 14}
	rec, store := recordRun(t,
		effectSpec{"model", "a", small, nil},
		effectSpec{"model", "b", large, chunks},
	)
	path := exportTo(t, "run.zip", rec, store, trace.WithSpillThreshold(16))

	fresh := vfs.NewMemStore()
	bundle, err := trace.ImportFile(path, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Recording.Effects[1].ValueRef == "" {
		t.Fatal("expected the large value to be referenced")
	}

	rr, err := trace.Replay(fresh, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	val, got, err := rr.ReplayEffectChunks("model", "a")
	if err != nil || !bytes.Equal(val, small) || got != nil {
		t.Fatalf("unexpected inline replay %q %v %v", val, got, err)
	}
	val, got, err = rr.ReplayEffectChunks("model", "b")
	if err != nil || !bytes.Equal(val, large) || !reflect.DeepEqual(got, chunks) {
		t.Fatalf("unexpected spilled replay %q %v %v", val, got, err)
	}
}

func TestExportIsDeterministic(t *testing.T) {
	specs := []effectSpec{
		{"model", "a", filler(100, 'a'), []int{40, 60}},
		{"http", "b", filler(100, 'b'), nil},
		{"tool:x", "c", []byte("small"), nil},
	}
	rec1, store1 := recordRun(t, specs...)
	rec2, store2 := recordRun(t, specs...)

	a, err := os.ReadFile(exportTo(t, "a.zip", rec1, store1, trace.WithSpillThreshold(50)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exportTo(t, "b.zip", rec2, store2, trace.WithSpillThreshold(50)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := os.ReadFile(exportTo(t, "c.zip", rec1, store1, trace.WithSpillThreshold(50)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatal("export is not byte-for-byte deterministic")
	}
}

func TestReexportKeepsRefs(t *testing.T) {
	rec, store := recordRun(t,
		effectSpec{"model", "a", filler(100, 'a'), []int{50, 50}},
		effectSpec{"model", "b", []byte("small"), nil},
	)
	first := exportTo(t, "first.zip", rec, store, trace.WithSpillThreshold(50))
	bundle := importFrom(t, first)

	for name, opts := range map[string][]trace.ExportOption{
		"default": nil,
		"same":    {trace.WithSpillThreshold(50)},
		"never":   {trace.WithSpillThreshold(-1)},
		"huge":    {trace.WithSpillThreshold(1 << 30)},
	} {
		again := exportTo(t, "again.zip", bundle.Recording, bundle.Store, opts...)
		a, err := os.ReadFile(first) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}

		b, err := os.ReadFile(again) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: re-export changed the bundle", name)
		}
	}
}

func TestSpillDeduplicatesIdenticalValues(t *testing.T) {
	value := filler(100, 'd')
	one, store1 := recordRun(t, effectSpec{"model", "a", value, nil})
	two, store2 := recordRun(t,
		effectSpec{"model", "a", value, nil},
		effectSpec{"http", "b", value, nil},
	)

	oneFiles := readZip(t, exportTo(t, "one.zip", one, store1, trace.WithSpillThreshold(10)))
	twoFiles := readZip(t, exportTo(t, "two.zip", two, store2, trace.WithSpillThreshold(10)))
	m := manifestOf(t, twoFiles)
	if m.Effects[0].ValueRef == "" || m.Effects[0].ValueRef != m.Effects[1].ValueRef {
		t.Fatalf("identical values must share one reference: %+v", m.Effects)
	}
	if len(objectNames(oneFiles)) != len(objectNames(twoFiles)) {
		t.Fatalf("duplicate value stored twice: %d vs %d objects", len(objectNames(oneFiles)), len(objectNames(twoFiles)))
	}
}

func TestOldInlineBundleImportsReplaysAndRedacts(t *testing.T) {
	rec, store := recordRun(t)
	files := readZip(t, exportTo(t, "base.zip", rec, store))
	files["trace.json"] = []byte(fmt.Sprintf(`{"version":1,"hash_algorithm":"blake3","producer":"zygote-cli","has_raw_prompts":true,`+
		`"steps":[{"n":0,"name":"init","root":%q,"effects":0},{"n":1,"name":"end","root":%q,"effects":3}],`+
		`"effects":[{"seq":0,"op":"model","key":"k","value":"aGVsbG8="},{"seq":1,"op":"clock","key":"now","value":""},{"seq":2,"op":"rand","key":"seed","value":null}]}`,
		rec.Steps[0].Root, rec.Steps[1].Root))
	path := writeZip(t, files)

	store2 := vfs.NewMemStore()
	bundle, err := trace.ImportFile(path, store2)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := trace.Replay(store2, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"hello", "", ""} {
		e := bundle.Recording.Effects[i]
		val, chunks, err := rr.ReplayEffectChunks(e.Op, e.Key)
		if err != nil || string(val) != want || chunks != nil {
			t.Fatalf("effect %d: %q %v %v", i, val, chunks, err)
		}
	}

	clean, err := trace.RedactBundle(&bundle, redactNothing())
	if err != nil {
		t.Fatal(err)
	}
	if string(clean.Recording.Effects[0].Value) != "hello" || !clean.Recording.HasRawPrompts {
		t.Fatalf("unexpected redaction result %+v", clean.Recording.Effects[0])
	}

	out := manifestOf(t, readZip(t, exportTo(t, "out.zip", clean.Recording, clean.Store)))
	for _, e := range out.Effects {
		if e.ValueRef != "" || e.ValueSize != 0 || e.Chunks != nil {
			t.Fatalf("old-style effects must stay inline: %+v", e)
		}
	}
	if string(out.Effects[0].Value) != "hello" {
		t.Fatalf("inline value lost: %+v", out.Effects[0])
	}
}

func spilledFixture(t *testing.T) (string, string) {
	t.Helper()
	rec, store := recordRun(t,
		effectSpec{"model", "small", []byte("tiny"), nil},
		effectSpec{"model", "big", filler(20, 'b'), []int{10, 10}},
	)
	path := exportTo(t, "fixture.zip", rec, store, trace.WithSpillThreshold(10))
	return path, hashOf(t, filler(20, 'b'))
}

func TestImportRejectsTamperedEffectObject(t *testing.T) {
	path, ref := spilledFixture(t)
	files := readZip(t, path)
	files["objects/"+ref] = filler(20, 'x')

	_, err := trace.ImportFile(writeZip(t, files), vfs.NewMemStore())
	if !errors.Is(err, trace.ErrHashMismatch) {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestImportRejectsDeletedEffectObject(t *testing.T) {
	path, ref := spilledFixture(t)
	files := readZip(t, path)
	delete(files, "objects/"+ref)

	_, err := trace.ImportFile(writeZip(t, files), vfs.NewMemStore())
	if !errors.Is(err, trace.ErrMissingObject) {
		t.Fatalf("expected missing object, got %v", err)
	}
}

func TestImportRejectsMalformedEffects(t *testing.T) {
	path, ref := spilledFixture(t)

	cases := map[string]func(*trace.Recording){
		"value and ref":        func(r *trace.Recording) { r.Effects[1].Value = []byte("x") },
		"non-hex ref":          func(r *trace.Recording) { r.Effects[1].ValueRef = strings.Repeat("z", 64) },
		"short ref":            func(r *trace.Recording) { r.Effects[1].ValueRef = ref[:62] },
		"long ref":             func(r *trace.Recording) { r.Effects[1].ValueRef = ref + "00" },
		"uppercase ref":        func(r *trace.Recording) { r.Effects[1].ValueRef = strings.ToUpper(ref) },
		"prefixed ref":         func(r *trace.Recording) { r.Effects[1].ValueRef = "blake3:" + ref[:57] },
		"chunk sum short":      func(r *trace.Recording) { r.Effects[1].Chunks = []int{5, 5} },
		"chunk sum long":       func(r *trace.Recording) { r.Effects[1].Chunks = []int{10, 11} },
		"chunk zero":           func(r *trace.Recording) { r.Effects[1].Chunks = []int{0, 20} },
		"chunk negative":       func(r *trace.Recording) { r.Effects[1].Chunks = []int{-1, 21} },
		"inline chunk sum":     func(r *trace.Recording) { r.Effects[0].Chunks = []int{1, 1} },
		"inline chunk zero":    func(r *trace.Recording) { r.Effects[0].Chunks = []int{0, 4} },
		"inline chunk neg":     func(r *trace.Recording) { r.Effects[0].Chunks = []int{-1, 5} },
		"size mismatch":        func(r *trace.Recording) { r.Effects[1].ValueSize = 3 },
		"negative size":        func(r *trace.Recording) { r.Effects[1].ValueSize = -20 },
		"size without ref":     func(r *trace.Recording) { r.Effects[0].ValueSize = 4 },
		"ref to absent object": func(r *trace.Recording) { r.Effects[1].ValueRef = strings.Repeat("0", 64) },
	}
	for name, edit := range cases {
		files := readZip(t, path)
		editManifest(t, files, edit)
		_, err := trace.ImportFile(writeZip(t, files), vfs.NewMemStore())
		if err == nil {
			t.Errorf("%s: import accepted a malformed effect", name)
			continue
		}
		if name == "ref to absent object" {
			if !errors.Is(err, trace.ErrMissingObject) {
				t.Errorf("%s: expected missing object, got %v", name, err)
			}
			continue
		}
		if !errors.Is(err, trace.ErrInvalidEffect) {
			t.Errorf("%s: expected invalid effect, got %v", name, err)
		}
	}
}

func TestImportAcceptsValidFixture(t *testing.T) {
	path, ref := spilledFixture(t)
	b := importFrom(t, path)
	if b.Recording.Effects[1].ValueRef != ref || b.Recording.Effects[1].ValueSize != 20 {
		t.Fatalf("unexpected entry %+v", b.Recording.Effects[1])
	}
}

func TestExportRejectsInvalidEffects(t *testing.T) {
	rec, store := recordRun(t, effectSpec{"model", "a", []byte("hello"), nil})

	bad := rec
	bad.Effects = []journal.Entry{{Seq: 0, Op: "model", Key: "a", Value: []byte("hello"), Chunks: []int{1, 1}}}
	path := filepath.Join(t.TempDir(), "bad.zip")
	if err := trace.ExportFile(path, store, bad); !errors.Is(err, trace.ErrInvalidEffect) {
		t.Fatalf("expected invalid effect, got %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a rejected export must not leave a file behind")
	}

	missing := rec
	missing.Effects = []journal.Entry{{Seq: 0, Op: "model", Key: "a", ValueRef: strings.Repeat("1", 64)}}
	if err := trace.ExportFile(filepath.Join(t.TempDir(), "m.zip"), store, missing); !errors.Is(err, trace.ErrMissingObject) {
		t.Fatalf("expected missing object, got %v", err)
	}
}

type lyingStore struct {
	vfs.Store
	target string
}

func (s lyingStore) Get(h vfs.Hash) ([]byte, bool) {
	b, ok := s.Store.Get(h)
	if ok && hex.EncodeToString(h[:]) == s.target {
		return filler(len(b), 'z'), true
	}
	return b, ok
}

func TestReplayFailsClosedOnUnresolvableValue(t *testing.T) {
	path, ref := spilledFixture(t)
	good := importFrom(t, path)

	run := func(store vfs.Store, rec trace.Recording) error {
		bundle := trace.Bundle{Recording: rec, Store: store}
		rr, err := trace.Replay(store, &bundle)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rr.ReplayEffect("model", "small"); err != nil {
			t.Fatal(err)
		}
		val, err := rr.ReplayEffect("model", "big")
		if err != nil && val != nil {
			t.Fatalf("an unresolvable value must never be returned, got %q", val)
		}
		return err
	}

	if err := run(good.Store, good.Recording); err != nil {
		t.Fatalf("control replay failed: %v", err)
	}
	asDivergence(t, run(vfs.NewMemStore(), good.Recording), journal.ClassMissingObject)
	asDivergence(t, run(lyingStore{Store: good.Store, target: ref}, good.Recording), journal.ClassMissingObject)

	wrongSize := good.Recording
	wrongSize.Effects = append([]journal.Entry(nil), good.Recording.Effects...)
	wrongSize.Effects[1].ValueSize++
	asDivergence(t, run(good.Store, wrongSize), journal.ClassMissingObject)

	badRef := good.Recording
	badRef.Effects = append([]journal.Entry(nil), good.Recording.Effects...)
	badRef.Effects[1].ValueRef = "not-a-hash"
	asDivergence(t, run(good.Store, badRef), journal.ClassMissingObject)
}

func TestAutoStepStreamedToolEffect(t *testing.T) {
	r, store := newAuto(t)
	value := []byte("abcdefgh")

	got, step, err := r.RecordEffectChunks("tool:search", "q", func() ([]byte, []int, error) { return value, []int{5, 3}, nil })
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("unexpected result %q %v", got, err)
	}
	if step == nil || step.Name != "auto:tool:search" || step.Effects != 1 {
		t.Fatalf("a streamed tool effect must close an auto step: %+v", step)
	}
	if !reflect.DeepEqual(r.Recording().Effects[0].Chunks, []int{5, 3}) {
		t.Fatalf("chunks not recorded: %+v", r.Recording().Effects[0])
	}

	if _, step, err := r.RecordEffectChunks("model", "p", func() ([]byte, []int, error) { return value, []int{4, 4}, nil }); err != nil || step != nil {
		t.Fatalf("a streamed model effect must not close a step: %+v %v", step, err)
	}
	r.Finish()

	rr := replayOf(t, r, store)
	val, chunks, err := rr.ReplayEffectChunks("tool:search", "q")
	if err != nil || !bytes.Equal(val, value) || !reflect.DeepEqual(chunks, []int{5, 3}) {
		t.Fatalf("unexpected replay %q %v %v", val, chunks, err)
	}
	if _, chunks, err = rr.ReplayEffectChunks("model", "p"); err != nil || !reflect.DeepEqual(chunks, []int{4, 4}) {
		t.Fatalf("unexpected model replay %v %v", chunks, err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("replay diverged: %v", err)
	}
}

func TestAutoStepStreamedToolEffectSurvivesSpill(t *testing.T) {
	r, store := newAuto(t)
	value := filler(64, 'v')
	if _, _, err := r.RecordEffectChunks("tool:search", "q", func() ([]byte, []int, error) { return value, []int{32, 32}, nil }); err != nil {
		t.Fatal(err)
	}
	r.Finish()

	path := exportTo(t, "auto.zip", r.Recording(), store, trace.WithSpillThreshold(8))
	fresh := vfs.NewMemStore()
	bundle, err := trace.ImportFile(path, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Recording.Effects[0].ValueRef == "" {
		t.Fatal("expected a spilled value")
	}
	rr, err := trace.Replay(fresh, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	val, chunks, err := rr.ReplayEffectChunks("tool:search", "q")
	if err != nil || !bytes.Equal(val, value) || !reflect.DeepEqual(chunks, []int{32, 32}) {
		t.Fatalf("unexpected replay %q %v %v", val, chunks, err)
	}
	if err := rr.Finish(); err != nil {
		t.Fatalf("auto-step replay diverged: %v", err)
	}
}
