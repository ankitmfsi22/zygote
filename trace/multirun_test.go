package trace_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
)

// recordVariant records a run that shares shared.txt with every other variant and owns own.txt.
func recordVariant(t *testing.T, own string, specs ...effectSpec) (trace.Recording, vfs.Store) {
	t.Helper()
	store := vfs.NewMemStore()
	r := trace.Record(store)
	if err := r.World().WriteFile("shared.txt", []byte("shared by every run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.World().WriteFile("own.txt", []byte(own), 0o644); err != nil {
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

// mergeFixture exports three bundles named a.zip, b.zip and c.zip into one directory: a has a spilled effect,
// b only inline effects and c is an auto-step recording with a spilled streamed tool effect.
func mergeFixture(t *testing.T, opts ...trace.ExportOption) (dir string, inputs []string) {
	t.Helper()
	dir = t.TempDir()
	export := func(name string, rec trace.Recording, store vfs.Store) {
		path := filepath.Join(dir, name)
		if err := trace.ExportFile(path, store, rec, append([]trace.ExportOption{trace.WithSpillThreshold(16)}, opts...)...); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, path)
	}

	recA, storeA := recordVariant(t, "alpha", effectSpec{op: "model", key: "p", value: filler(64, 'a'), chunks: []int{32, 32}})
	export("a.zip", recA, storeA)
	recB, storeB := recordVariant(t, "beta", effectSpec{op: "tool", key: "k", value: []byte("tiny")})
	export("b.zip", recB, storeB)

	storeC := vfs.NewMemStore()
	r := trace.Record(storeC, trace.WithAutoStep())
	if err := r.World().WriteFile("shared.txt", []byte("shared by every run"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Step("init")
	if _, _, err := r.RecordEffectChunks("tool:search", "q", func() ([]byte, []int, error) { return filler(64, 'c'), []int{40, 24}, nil }); err != nil {
		t.Fatal(err)
	}
	r.Finish()
	export("c.zip", r.Recording(), storeC)
	return dir, inputs
}

func mergeTo(t *testing.T, out string, inputs []string, imp []trace.ImportOption, exp []trace.ExportOption) trace.MergeStats {
	t.Helper()
	stats, err := trace.MergeFiles(out, inputs, imp, exp)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMergeSplitRoundTrip(t *testing.T) {
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "all.zip")
	stats := mergeTo(t, out, inputs, nil, nil)

	files := readZip(t, out)
	if _, ok := files["trace.json"]; ok {
		t.Fatal("a multi-run archive must not have a top-level trace.json")
	}
	var idx struct {
		Version int    `json:"version"`
		Format  string `json:"format"`
		Runs    []struct{ Name, Manifest string }
	}
	if err := json.Unmarshal(files["runs.json"], &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Version != 1 || idx.Format != "zygote-multi-run" || len(idx.Runs) != 3 {
		t.Fatalf("unexpected index: %+v", idx)
	}

	union := map[string]struct{}{}
	total := 0
	for _, in := range inputs {
		for _, name := range objectNames(readZip(t, in)) {
			union[name] = struct{}{}
			total++
		}
	}
	got := objectNames(files)
	if len(got) != len(union) || stats.Objects != len(union) {
		t.Fatalf("archive has %d objects (stats %d), the union is %d", len(got), stats.Objects, len(union))
	}
	if stats.Runs != 3 || stats.Shared == 0 || total <= len(union) {
		t.Fatalf("expected shared objects stored once, got stats %+v (total %d, union %d)", stats, total, len(union))
	}
	for _, name := range []string{"a", "b", "c"} {
		want := readZip(t, filepath.Join(dir, name+".zip"))["trace.json"]
		if !bytes.Equal(files["runs/"+name+"/trace.json"], want) {
			t.Fatalf("run %s manifest is not the original trace.json", name)
		}
	}

	t.Run("deterministic", func(t *testing.T) {
		again := filepath.Join(dir, "again.zip")
		reversed := slices.Clone(inputs)
		slices.Reverse(reversed)
		mergeTo(t, again, reversed, nil, nil)
		if !bytes.Equal(readFile(t, out), readFile(t, again)) {
			t.Fatal("same inputs produced different archives")
		}
	})

	t.Run("split is byte-identical", func(t *testing.T) {
		outDir := filepath.Join(t.TempDir(), "split")
		written, err := trace.SplitFile(out, outDir, trace.SplitParams{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(written) != 3 {
			t.Fatalf("wrote %v", written)
		}
		for _, name := range []string{"a", "b", "c"} {
			orig := readFile(t, filepath.Join(dir, name+".zip"))
			if !bytes.Equal(orig, readFile(t, filepath.Join(outDir, name+".zip"))) {
				t.Fatalf("split output %s.zip differs from the original bundle", name)
			}
		}
	})

	t.Run("replay and effects survive", func(t *testing.T) {
		for _, name := range []string{"a", "c"} {
			b, err := trace.ImportRun(out, name, vfs.NewMemStore())
			if err != nil {
				t.Fatal(err)
			}
			orig := importFrom(t, filepath.Join(dir, name+".zip"))
			if !reflect.DeepEqual(b.Recording, orig.Recording) {
				t.Fatalf("run %s recording differs after merge", name)
			}
			if b.Recording.Effects[0].ValueRef == "" {
				t.Fatalf("run %s lost its spilled effect", name)
			}
			rr, err := trace.Replay(b.Store, &b)
			if err != nil {
				t.Fatal(err)
			}
			op, key := "model", "p"
			if name == "c" {
				op, key = "tool:search", "q"
			}
			val, chunks, err := rr.ReplayEffectChunks(op, key)
			if err != nil || len(val) != 64 || len(chunks) != 2 {
				t.Fatalf("run %s: replay %v %v %v", name, len(val), chunks, err)
			}
			if err := rr.Finish(); err != nil {
				t.Fatalf("run %s diverged: %v", name, err)
			}
		}
	})
}

func TestMergeFlattensArchives(t *testing.T) {
	dir, inputs := mergeFixture(t)
	ab := filepath.Join(dir, "ab.zip")
	mergeTo(t, ab, inputs[:2], nil, nil)
	all := filepath.Join(dir, "all.zip")
	stats := mergeTo(t, all, []string{ab, inputs[2]}, nil, nil)
	direct := filepath.Join(dir, "direct.zip")
	mergeTo(t, direct, inputs, nil, nil)
	if stats.Runs != 3 || !bytes.Equal(readFile(t, all), readFile(t, direct)) {
		t.Fatalf("merging an archive with a bundle must equal merging the bundles: %+v", stats)
	}
}

func TestMergeEncrypted(t *testing.T) {
	inID, inFile := newIdentity(t)
	outID, outFile := newIdentity(t)
	thirdID, thirdFile := newIdentity(t)
	dir, inputs := mergeFixture(t, trace.WithRecipients(inID.Recipient().String()))
	plain := filepath.Join(t.TempDir(), "plain.zip")
	_, err := trace.MergeFiles(plain, inputs, nil, nil)
	var inErr *trace.InputError
	if !errors.Is(err, trace.ErrEncrypted) || !errors.As(err, &inErr) {
		t.Fatalf("encrypted inputs without a key: %v", err)
	}
	if fileExists(plain) {
		t.Fatal("a failed merge wrote output")
	}

	imp := []trace.ImportOption{trace.WithIdentityFile(inFile)}
	mergeTo(t, plain, inputs, imp, nil)
	if enc, err := trace.IsEncrypted(plain); err != nil || enc {
		t.Fatalf("output must be unencrypted without --encrypt: %v %v", enc, err)
	}

	secret := filepath.Join(dir, "secret.zip")
	mergeTo(t, secret, inputs, imp, []trace.ExportOption{trace.WithRecipients(outID.Recipient().String())})
	if enc, err := trace.IsEncrypted(secret); err != nil || !enc {
		t.Fatalf("expected encrypted output: %v %v", enc, err)
	}
	if _, err := trace.OpenArchive(secret); !errors.Is(err, trace.ErrEncrypted) {
		t.Fatalf("open without key: %v", err)
	}
	if _, err := trace.OpenArchive(secret, imp...); !errors.Is(err, trace.ErrDecrypt) {
		t.Fatalf("open with the wrong key: %v", err)
	}
	other := []trace.ImportOption{trace.WithIdentityFile(outFile)}
	runs, err := trace.ListRuns(secret, other...)
	if err != nil || !slices.Equal(runs, []string{"a", "b", "c"}) {
		t.Fatalf("ListRuns: %v %v", runs, err)
	}

	split := filepath.Join(t.TempDir(), "split")
	if _, err := trace.SplitFile(secret, split, trace.SplitParams{Runs: []string{"b"}}, other, nil); err != nil {
		t.Fatal(err)
	}
	if enc, _ := trace.IsEncrypted(filepath.Join(split, "b.zip")); enc {
		t.Fatal("split output must be unencrypted without --encrypt")
	}
	got, err := trace.ImportFile(filepath.Join(split, "b.zip"), vfs.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	orig, err := trace.ImportFile(inputs[1], vfs.NewMemStore(), imp...)
	if err != nil || !reflect.DeepEqual(got.Recording, orig.Recording) {
		t.Fatalf("split run differs from the encrypted original: %v", err)
	}
	enc := filepath.Join(t.TempDir(), "enc")
	if _, err := trace.SplitFile(secret, enc, trace.SplitParams{}, other, []trace.ExportOption{trace.WithRecipients(thirdID.Recipient().String())}); err != nil {
		t.Fatal(err)
	}
	if isEnc, _ := trace.IsEncrypted(filepath.Join(enc, "a.zip")); !isEnc {
		t.Fatal("split --encrypt output must be encrypted")
	}
	if _, err := trace.ImportFile(filepath.Join(enc, "a.zip"), vfs.NewMemStore(), trace.WithIdentityFile(thirdFile)); err != nil {
		t.Fatal(err)
	}
}

// TestMergePassphrase is the only merge test that uses (slow) passphrase encryption: one encrypt and one decrypt.
func TestMergePassphrase(t *testing.T) {
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "pass.zip")
	mergeTo(t, out, inputs, nil, []trace.ExportOption{trace.WithEncryptionPassphrase("merge-test-passphrase")})
	runs, err := trace.ListRuns(out, trace.WithPassphrase("merge-test-passphrase"))
	if err != nil || !slices.Equal(runs, []string{"a", "b", "c"}) {
		t.Fatalf("ListRuns: %v %v", runs, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestImportFileRejectsMultiRunArchive(t *testing.T) {
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "all.zip")
	mergeTo(t, out, inputs, nil, nil)

	_, err := trace.ImportFile(out, vfs.NewMemStore())
	if !errors.Is(err, trace.ErrMultiRunArchive) {
		t.Fatalf("got %v, want ErrMultiRunArchive", err)
	}
	for _, want := range []string{"3 runs", "zyg split", "zyg verify --corpus"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err, want)
		}
	}
	if _, err := trace.ListRuns(inputs[0]); !errors.Is(err, trace.ErrSingleRunBundle) {
		t.Fatalf("ListRuns on a single-run bundle: %v", err)
	}
	if _, err := trace.ImportRun(out, "nope", vfs.NewMemStore()); !errors.Is(err, trace.ErrUnknownRun) {
		t.Fatalf("ImportRun with an unknown run: %v", err)
	}
}

func TestMergeErrors(t *testing.T) {
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "out.zip")

	t.Run("fewer than two runs", func(t *testing.T) {
		if _, err := trace.MergeFiles(out, inputs[:1], nil, nil); err == nil || !strings.Contains(err.Error(), "at least 2 runs") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("duplicate run name", func(t *testing.T) {
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o750); err != nil {
			t.Fatal(err)
		}
		dup := filepath.Join(sub, "a.zip")
		if err := os.WriteFile(dup, readFile(t, inputs[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := trace.MergeFiles(out, []string{inputs[0], dup}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), inputs[0]) || !strings.Contains(err.Error(), dup) {
			t.Fatalf("the error must name both inputs: %v", err)
		}
	})
	t.Run("invalid run name", func(t *testing.T) {
		bad := filepath.Join(dir, "has space.zip")
		if err := os.WriteFile(bad, readFile(t, inputs[0]), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := trace.MergeFiles(out, []string{bad, inputs[1]}, nil, nil); !errors.Is(err, trace.ErrInvalidRunName) {
			t.Fatalf("got %v", err)
		}
	})
	if fileExists(out) {
		t.Fatal("a failed merge wrote output")
	}
}

// multiParts is an unpacked multi-run archive that tests can tamper with.
func multiParts(t *testing.T) map[string][]byte {
	t.Helper()
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "all.zip")
	mergeTo(t, out, inputs, nil, nil)
	return readZip(t, out)
}

func TestMultiRunFailsClosed(t *testing.T) {
	setIndex := func(files map[string][]byte, edit func(map[string]any)) {
		var m map[string]any
		if err := json.Unmarshal(files["runs.json"], &m); err != nil {
			panic(err)
		}
		edit(m)
		b, err := json.Marshal(m)
		if err != nil {
			panic(err)
		}
		files["runs.json"] = b
	}
	run := func(m map[string]any, i int) map[string]any { return m["runs"].([]any)[i].(map[string]any) }

	tests := []struct {
		name   string
		tamper func(files map[string][]byte)
		want   error
		text   string
	}{
		{"object hash mismatch", func(f map[string][]byte) {
			f[objectNames(f)[0]] = []byte("corrupted")
		}, trace.ErrHashMismatch, ""},
		{"object missing", func(f map[string][]byte) {
			delete(f, objectNames(f)[0])
		}, nil, "run"},
		{"object name not a hash", func(f map[string][]byte) {
			f["objects/zz"] = []byte("x")
		}, trace.ErrInvalidArchive, "objects/zz"},
		{"unknown field", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) { m["extra"] = 1 })
		}, trace.ErrInvalidArchive, "extra"},
		{"wrong version", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) { m["version"] = 2 })
		}, trace.ErrInvalidArchive, "version"},
		{"wrong format", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) { m["format"] = "something-else" })
		}, trace.ErrInvalidArchive, "format"},
		{"no runs", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) { m["runs"] = []any{} })
		}, trace.ErrInvalidArchive, "no runs"},
		{"bad manifest path", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) { run(m, 0)["manifest"] = "runs/other/trace.json" })
		}, trace.ErrInvalidArchive, "manifest"},
		{"duplicate run name", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) {
				run(m, 1)["name"] = "a"
				run(m, 1)["manifest"] = "runs/a/trace.json"
			})
		}, trace.ErrInvalidArchive, "duplicate"},
		{"invalid run name", func(f map[string][]byte) {
			setIndex(f, func(m map[string]any) {
				run(m, 0)["name"] = "../x"
				run(m, 0)["manifest"] = "runs/../x/trace.json"
			})
		}, trace.ErrInvalidRunName, ""},
		{"overlong run name", func(f map[string][]byte) {
			long := strings.Repeat("n", 129)
			setIndex(f, func(m map[string]any) {
				run(m, 0)["name"] = long
				run(m, 0)["manifest"] = "runs/" + long + "/trace.json"
			})
		}, trace.ErrInvalidRunName, ""},
		{"missing manifest", func(f map[string][]byte) {
			delete(f, "runs/b/trace.json")
		}, trace.ErrInvalidArchive, "missing runs/b/trace.json"},
		{"unexpected runs entry", func(f map[string][]byte) {
			f["runs/b/extra.txt"] = []byte("x")
		}, trace.ErrInvalidArchive, "unexpected entry"},
		{"unexpected unlisted run", func(f map[string][]byte) {
			f["runs/z/trace.json"] = f["runs/a/trace.json"]
		}, trace.ErrInvalidArchive, "unexpected entry"},
		{"top-level trace.json", func(f map[string][]byte) {
			f["trace.json"] = f["runs/a/trace.json"]
		}, trace.ErrInvalidArchive, "trace.json"},
		{"recording version", func(f map[string][]byte) {
			var rec trace.Recording
			if err := json.Unmarshal(f["runs/b/trace.json"], &rec); err != nil {
				panic(err)
			}
			rec.Version = 2
			f["runs/b/trace.json"], _ = json.Marshal(rec)
		}, nil, "unsupported format version: 2"},
		{"unreachable step root", func(f map[string][]byte) {
			var rec trace.Recording
			if err := json.Unmarshal(f["runs/b/trace.json"], &rec); err != nil {
				panic(err)
			}
			rec.Steps[1].Root = strings.Repeat("ab", 32)
			f["runs/b/trace.json"], _ = json.Marshal(rec)
		}, trace.ErrMissingObject, ""},
		{"missing spilled effect object", func(f map[string][]byte) {
			var rec trace.Recording
			if err := json.Unmarshal(f["runs/a/trace.json"], &rec); err != nil {
				panic(err)
			}
			delete(f, "objects/"+rec.Effects[0].ValueRef)
		}, trace.ErrMissingObject, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := multiParts(t)
			tt.tamper(files)
			path := writeZip(t, files)
			_, err := trace.OpenArchive(path)
			if err == nil {
				t.Fatal("tampered archive was accepted")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("error %q lacks %q", err, tt.text)
			}
			if _, err := trace.ListRuns(path); err == nil {
				t.Fatal("ListRuns accepted a tampered archive")
			}
			if _, err := trace.ImportRun(path, "a", vfs.NewMemStore()); err == nil {
				t.Fatal("ImportRun accepted a tampered archive")
			}
			if _, err := trace.MergeFiles(filepath.Join(t.TempDir(), "o.zip"), []string{path, path}, nil, nil); err == nil {
				t.Fatal("MergeFiles accepted a tampered archive")
			}
			outDir := filepath.Join(t.TempDir(), "split")
			if _, err := trace.SplitFile(path, outDir, trace.SplitParams{}, nil, nil); err == nil || fileExists(outDir) {
				t.Fatalf("SplitFile must fail without writing: %v", err)
			}
		})
	}
}

func TestSplitErrors(t *testing.T) {
	dir, inputs := mergeFixture(t)
	out := filepath.Join(dir, "all.zip")
	mergeTo(t, out, inputs, nil, nil)
	outDir := filepath.Join(t.TempDir(), "split")

	if _, err := trace.SplitFile(inputs[0], outDir, trace.SplitParams{}, nil, nil); !errors.Is(err, trace.ErrSingleRunBundle) {
		t.Fatalf("split on a single-run bundle: %v", err)
	}
	if _, err := trace.SplitFile(out, outDir, trace.SplitParams{Runs: []string{"a", "nope"}}, nil, nil); !errors.Is(err, trace.ErrUnknownRun) || fileExists(outDir) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := trace.SplitFile(out, outDir, trace.SplitParams{Runs: []string{"a", "a"}}, nil, nil); err != nil {
		t.Fatalf("a repeated --run is harmless: %v", err)
	}
	if _, err := trace.SplitFile(out, outDir, trace.SplitParams{Runs: []string{"a"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite without force: %v", err)
	}
	if _, err := trace.SplitFile(out, outDir, trace.SplitParams{Force: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.Glob(filepath.Join(outDir, ".zyg-*")); len(got) != 0 {
		t.Fatalf("temp files left behind: %v", got)
	}
}
