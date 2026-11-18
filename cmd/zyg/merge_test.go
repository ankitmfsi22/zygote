package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const mergeAgentOps = "effect|model|mode|" + b64Ok

// bundleDir records one bundle per name (all with the same agent) into a fresh directory.
func bundleDir(t *testing.T, names ...string) string {
	t.Helper()
	requireShell(t)
	agent := fakeAgent(t, mergeAgentOps, "step|plan")
	dir := t.TempDir()
	for _, name := range names {
		copyFile(t, recordFake(t, nil, agent), filepath.Join(dir, name))
	}
	return dir
}

// mergedArchive merges the named bundles into <dir>/all.zip and returns the archive path.
func mergedArchive(t *testing.T, dir string, names ...string) string {
	t.Helper()
	out := filepath.Join(dir, "all.zip")
	args := []string{"merge", "-o", out}
	for _, n := range names {
		args = append(args, filepath.Join(dir, n))
	}
	if res := runZygSplit(t, nil, args...); res.code != 0 {
		t.Fatalf("merge exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	return out
}

func readFileBytes(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewriteZip copies a zip, passing every entry through edit; edit returns the new content.
func rewriteZip(t testing.TB, src, dst string, edit func(name string, data []byte) []byte) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(edit(f.Name, data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func corruptFirstObject(t testing.TB, src, dst string) {
	t.Helper()
	done := false
	rewriteZip(t, src, dst, func(name string, data []byte) []byte {
		if !done && strings.HasPrefix(name, "objects/") {
			done = true
			return []byte("corrupted")
		}
		return data
	})
	if !done {
		t.Fatal("archive has no objects to corrupt")
	}
}

func dirEntries(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMergeSplitCLI(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip", "c.zip")
		out := filepath.Join(dir, "all.zip")
		res := runZygSplit(t, nil, "merge", "-o", out, filepath.Join(dir, "c.zip"), filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip"))
		// Every recording has the same world (a root tree and prompt.txt), so both objects are stored once and shared by all runs.
		if want := "merged 3 runs into " + out + " (2 objects, 2 shared)\n"; res.code != 0 || res.stdout != want {
			t.Fatalf("exit %d: stdout %q, want %q\nstderr: %s", res.code, res.stdout, want, res.stderr)
		}

		split := filepath.Join(t.TempDir(), "out", "nested")
		res = runZygSplit(t, nil, "split", out, "-o", split)
		if res.code != 0 || !strings.Contains(res.stdout, "split 3 runs from") {
			t.Fatalf("split exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		for _, n := range []string{"a.zip", "b.zip", "c.zip"} {
			if !bytes.Equal(readFileBytes(t, filepath.Join(dir, n)), readFileBytes(t, filepath.Join(split, n))) {
				t.Fatalf("%s: split output differs from the original bundle", n)
			}
		}

		only := filepath.Join(t.TempDir(), "only")
		if res := runZygSplit(t, nil, "split", out, "-o", only, "--run", "b", "--run", "c"); res.code != 0 {
			t.Fatalf("split --run exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		if got := dirEntries(t, only); !slices.Equal(got, []string{"b.zip", "c.zip"}) {
			t.Fatalf("split --run wrote %v", got)
		}

		// A split bundle replays like the original.
		agent := fakeAgent(t, mergeAgentOps, "step|plan")
		if res := runZygSplit(t, nil, "replay", filepath.Join(split, "b.zip"), "--agent", agent); res.code != 0 {
			t.Fatalf("replay exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("merge flattens archives", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip", "c.zip")
		ab := filepath.Join(dir, "ab.zip")
		if res := runZygSplit(t, nil, "merge", "-o", ab, filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")); res.code != 0 {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		all := filepath.Join(dir, "all.zip")
		res := runZygSplit(t, nil, "merge", "-o", all, ab, filepath.Join(dir, "c.zip"))
		if res.code != 0 || !strings.HasPrefix(res.stdout, "merged 3 runs into") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("duplicate run names", func(t *testing.T) {
		one, two := bundleDir(t, "a.zip"), bundleDir(t, "a.zip")
		out := filepath.Join(t.TempDir(), "out.zip")
		res := runZygSplit(t, nil, "merge", "-o", out, filepath.Join(one, "a.zip"), filepath.Join(two, "a.zip"))
		if res.code == 0 || !strings.Contains(res.stderr, filepath.Join(one, "a.zip")) || !strings.Contains(res.stderr, filepath.Join(two, "a.zip")) {
			t.Fatalf("exit %d: the error must name both inputs: %s", res.code, res.stderr)
		}
		if fileExists(out) {
			t.Fatal("output written")
		}
	})

	t.Run("invalid name", func(t *testing.T) {
		dir := bundleDir(t, "has space.zip", "b.zip")
		out := filepath.Join(t.TempDir(), "out.zip")
		res := runZygSplit(t, nil, "merge", "-o", out, filepath.Join(dir, "has space.zip"), filepath.Join(dir, "b.zip"))
		if res.code == 0 || !strings.Contains(res.stderr, "invalid run name") || fileExists(out) {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("fewer than two runs", func(t *testing.T) {
		dir := bundleDir(t, "a.zip")
		out := filepath.Join(t.TempDir(), "out.zip")
		res := runZygSplit(t, nil, "merge", "-o", out, filepath.Join(dir, "a.zip"))
		if res.code != 3 || !strings.Contains(res.stderr, "at least 2 runs") || fileExists(out) {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("output exists", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip")
		out := filepath.Join(t.TempDir(), "out.zip")
		if err := os.WriteFile(out, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"merge", "-o", out, filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")}
		res := runZygSplit(t, nil, args...)
		if res.code != 3 || !strings.Contains(res.stderr, "--force") || string(readFileBytes(t, out)) != "precious" {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
		if res := runZygSplit(t, nil, append(args, "--force")...); res.code != 0 {
			t.Fatalf("--force exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("output is an input", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip")
		a := filepath.Join(dir, "a.zip")
		before := readFileBytes(t, a)
		res := runZygSplit(t, nil, "merge", "-o", a, "--force", a, filepath.Join(dir, "b.zip"))
		if res.code != 3 || !strings.Contains(res.stderr, "refusing to overwrite input") || !bytes.Equal(before, readFileBytes(t, a)) {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("corrupt input writes nothing", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip")
		corruptFirstObject(t, filepath.Join(dir, "b.zip"), filepath.Join(dir, "bad.zip"))
		outDir := t.TempDir()
		res := runZygSplit(t, nil, "merge", "-o", filepath.Join(outDir, "out.zip"), filepath.Join(dir, "a.zip"), filepath.Join(dir, "bad.zip"))
		if res.code != 2 || !strings.Contains(res.stderr, "bad.zip") || !strings.Contains(res.stderr, "does not match its hash") {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
		if got := dirEntries(t, outDir); len(got) != 0 {
			t.Fatalf("a failed merge left %v behind", got)
		}
	})

	t.Run("split run unknown", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip")
		all := mergedArchive(t, dir, "a.zip", "b.zip")
		out := filepath.Join(t.TempDir(), "out")
		res := runZygSplit(t, nil, "split", all, "-o", out, "--run", "a", "--run", "nope")
		if res.code != 3 || !strings.Contains(res.stderr, `"nope"`) || fileExists(out) {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("split single-run bundle", func(t *testing.T) {
		dir := bundleDir(t, "a.zip")
		out := filepath.Join(t.TempDir(), "out")
		res := runZygSplit(t, nil, "split", filepath.Join(dir, "a.zip"), "-o", out)
		if res.code != 2 || !strings.Contains(res.stderr, "single-run bundle") || fileExists(out) {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("split refuses to overwrite", func(t *testing.T) {
		dir := bundleDir(t, "a.zip", "b.zip")
		all := mergedArchive(t, dir, "a.zip", "b.zip")
		out := t.TempDir()
		if err := os.WriteFile(filepath.Join(out, "b.zip"), []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runZygSplit(t, nil, "split", all, "-o", out)
		if res.code != 3 || !strings.Contains(res.stderr, "--force") || len(dirEntries(t, out)) != 1 {
			t.Fatalf("exit %d: %s", res.code, res.stderr)
		}
		if res := runZygSplit(t, nil, "split", all, "-o", out, "--force"); res.code != 0 {
			t.Fatalf("--force exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})

	t.Run("encryption", func(t *testing.T) {
		id, idFile := writeIdentity(t)
		pass := writePassphraseFile(t, "merge-pass")
		dir := t.TempDir()
		agent := fakeAgent(t, mergeAgentOps, "step|plan")
		for _, n := range []string{"a.zip", "b.zip"} {
			copyFile(t, recordFake(t, nil, agent, "--encrypt", "--recipient", id.Recipient().String()), filepath.Join(dir, n))
		}
		in := []string{filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")}
		out := filepath.Join(dir, "all.zip")

		if res := runZygSplit(t, nil, append([]string{"merge", "-o", out}, in...)...); res.code != 2 || !strings.Contains(res.stderr, "bundle is encrypted") || fileExists(out) {
			t.Fatalf("no key: exit %d: %s", res.code, res.stderr)
		}
		if res := runZygSplit(t, nil, append([]string{"merge", "-o", out, "--identity-file", idFile}, in...)...); res.code != 0 {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		if enc, err := isEncryptedFile(out); err != nil || enc {
			t.Fatalf("output encrypted without --encrypt: %v %v", enc, err)
		}

		secret := filepath.Join(dir, "secret.zip")
		env := []string{"ZYG_PASSPHRASE_FILE=" + pass}
		if res := runZygSplit(t, env, append([]string{"merge", "-o", secret, "--identity-file", idFile, "--encrypt", "--passphrase"}, in...)...); res.code != 0 {
			t.Fatalf("encrypted merge exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		if enc, err := isEncryptedFile(secret); err != nil || !enc {
			t.Fatalf("expected encrypted output: %v %v", enc, err)
		}
		split := filepath.Join(dir, "split")
		if res := runZygSplit(t, nil, "split", secret, "-o", split); res.code != 2 {
			t.Fatalf("split without key: exit %d: %s", res.code, res.stderr)
		}
		if res := runZygSplit(t, env, "split", secret, "-o", split); res.code != 0 {
			t.Fatalf("split exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		if enc, _ := isEncryptedFile(filepath.Join(split, "a.zip")); enc {
			t.Fatal("split output encrypted without --encrypt")
		}
	})
}

func isEncryptedFile(path string) (bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec
	return bytes.HasPrefix(b, []byte(ageHeader)), err
}

func TestReplayMultiRunArchiveError(t *testing.T) {
	dir := bundleDir(t, "a.zip", "b.zip", "c.zip")
	all := mergedArchive(t, dir, "a.zip", "b.zip", "c.zip")
	agent := fakeAgent(t, mergeAgentOps, "step|plan")

	for _, args := range [][]string{{"replay", all, "--agent", agent}, {"replay", all, "--agent", agent, "--json"}} {
		res := runZygSplit(t, nil, args...)
		if res.code != 2 {
			t.Fatalf("replay exit %d, want 2: %s%s", res.code, res.stdout, res.stderr)
		}
		out := res.stdout + res.stderr
		for _, want := range []string{"multi-run archive", "3 runs", "zyg split", "zyg verify --corpus"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q: %s", want, out)
			}
		}
		if strings.Contains(out, "unsupported format version") {
			t.Fatalf("unhelpful error: %s", out)
		}
	}

	res := runZygSplit(t, nil, "inspect", all, "--meta")
	if res.code != 2 || !strings.Contains(res.stderr, "multi-run archive") || !strings.Contains(res.stderr, "zyg split") {
		t.Fatalf("inspect exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
}
