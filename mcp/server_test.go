package mcp

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const awsKey = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec

func newSession(t *testing.T, root string, maxRead int) *mcpsdk.ClientSession {
	t.Helper()
	srv, err := NewServer(Options{Root: root, MaxRead: maxRead})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func rawCall(t *testing.T, cs *mcpsdk.ClientSession, name string, args any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func resultText(res *mcpsdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func call[T any](t *testing.T, cs *mcpsdk.ClientSession, name string, args any) T {
	t.Helper()
	res := rawCall(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, resultText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", name, b, err)
	}
	return out
}

func callErr(t *testing.T, cs *mcpsdk.ClientSession, name string, args any, want string) {
	t.Helper()
	res := rawCall(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s: expected tool error containing %q, got success", name, want)
	}
	if text := resultText(res); !strings.Contains(text, want) {
		t.Fatalf("%s: error %q does not contain %q", name, text, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //nolint:gosec
		t.Fatal(err)
	}
}

func importBundle(t *testing.T, path string) trace.Bundle {
	t.Helper()
	b, err := trace.ImportFile(path, vfs.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func snapOf(t *testing.T, hexHash string) vfs.Snapshot {
	t.Helper()
	snap, err := parseHash(hexHash)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func exportTwoStepBundle(t *testing.T, cs *mcpsdk.ClientSession, root string) (string, string) {
	t.Helper()
	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "a.txt", Content: "one"})
	step := call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	return filepath.Join(root, "run.zip"), step.WorldHash
}

func TestHappyPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "seed", "a.txt"), "hello")
	cs := newSession(t, root, 0)

	start := call[RunStartOutput](t, cs, "run.start", RunStartInput{Dir: "seed", Meta: map[string]string{"model": "m1"}})
	if start.RunID != "run-1" || len(start.WorldHash) != 64 {
		t.Fatalf("unexpected start output: %+v", start)
	}

	w := call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "src/b.txt", Content: "world"})
	s1 := call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "edit"})
	if s1.N != 1 || s1.WorldHash != w.WorldHash || s1.Effects != 0 {
		t.Fatalf("unexpected step 1: %+v", s1)
	}
	eff := call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "model", Key: "prompt", Value: "answer"})
	if eff.Seq != 0 {
		t.Fatalf("unexpected seq %d", eff.Seq)
	}
	s2 := call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "done"})
	if s2.N != 2 || s2.Effects != 1 {
		t.Fatalf("unexpected step 2: %+v", s2)
	}

	exp := call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	if exp.Steps != 3 || exp.Effects != 1 {
		t.Fatalf("unexpected export: %+v", exp)
	}

	b := importBundle(t, filepath.Join(root, "run.zip"))
	names := []string{}
	for _, s := range b.Recording.Steps {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"init", "edit", "done"}) {
		t.Fatalf("unexpected steps %v", names)
	}
	if b.Recording.Steps[0].Root != start.WorldHash || b.Recording.Steps[1].Root != s1.WorldHash || b.Recording.Steps[2].Root != s2.WorldHash {
		t.Fatal("bundle step hashes differ from the hashes reported by the tools")
	}
	if len(b.Recording.Effects) != 1 || b.Recording.Effects[0].Op != "model" || b.Recording.Effects[0].Key != "prompt" || string(b.Recording.Effects[0].Value) != "answer" {
		t.Fatalf("unexpected effects %+v", b.Recording.Effects)
	}
	if b.Recording.Meta["model"] != "m1" {
		t.Fatalf("unexpected meta %v", b.Recording.Meta)
	}
	data, err := vfs.Fork(b.Store, snapOf(t, s2.WorldHash)).ReadFile("src/b.txt")
	if err != nil || string(data) != "world" {
		t.Fatalf("bundle world read: %q %v", data, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, ".zyg-export-*")); len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestWrongOrderAndForce(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	const noRun = "no active run"
	callErr(t, cs, "run.step", RunStepInput{Name: "x"}, noRun)
	callErr(t, cs, "run.effect", RunEffectInput{Op: "o", Key: "k"}, noRun)
	callErr(t, cs, "run.input", RunInputInput{Name: "x", Files: []InputFile{{Path: "a", Content: "b"}}}, noRun)
	callErr(t, cs, "world.write", WorldWriteInput{Path: "a", Content: "b"}, noRun)
	callErr(t, cs, "world.read", WorldReadInput{Path: "."}, noRun)
	callErr(t, cs, "world.diff", WorldDiffInput{From: "0", To: "live"}, noRun)
	callErr(t, cs, "run.fork", RunForkInput{Step: 0, Into: "out"}, noRun)
	callErr(t, cs, "run.export", RunExportInput{Out: "run.zip"}, noRun)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	callErr(t, cs, "run.start", RunStartInput{}, "already active")
	again := call[RunStartOutput](t, cs, "run.start", RunStartInput{Force: true})
	if again.RunID != "run-2" {
		t.Fatalf("unexpected run id %s", again.RunID)
	}

	callErr(t, cs, "run.step", RunStepInput{}, "required")
	callErr(t, cs, "run.effect", RunEffectInput{Op: "", Key: "k", Value: "v"}, "required")
	callErr(t, cs, "run.effect", RunEffectInput{Op: "o", Key: "", Value: "v"}, "required")
	callErr(t, cs, "run.effect", RunEffectInput{Op: "o", Key: "k", Value: "!!", Encoding: "base64"}, "base64")
	callErr(t, cs, "run.effect", RunEffectInput{Op: "o", Key: "k", Value: "v", Encoding: "hex"}, "encoding")

	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	callErr(t, cs, "run.step", RunStepInput{Name: "late"}, "closed")
	callErr(t, cs, "run.effect", RunEffectInput{Op: "o", Key: "k", Value: "v"}, "closed")
	callErr(t, cs, "world.write", WorldWriteInput{Path: "a", Content: "b"}, "closed")
	call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "."})
}

func TestPathsOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "x.txt"), "x")
	cs := newSession(t, root, 0)

	const outside = "outside the allowed root"
	callErr(t, cs, "run.start", RunStartInput{Dir: ".."}, outside)
	callErr(t, cs, "run.start", RunStartInput{Dir: "sub/../../x"}, outside)
	callErr(t, cs, "run.start", RunStartInput{Dir: other}, outside)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	callErr(t, cs, "run.input", RunInputInput{Name: "in", FromDir: other}, outside)
	callErr(t, cs, "run.fork", RunForkInput{Step: 0, Into: "../escape"}, outside)
	callErr(t, cs, "run.fork", RunForkInput{Step: 0, Into: other}, outside)
	callErr(t, cs, "run.export", RunExportInput{Out: "../run.zip"}, outside)
	callErr(t, cs, "run.export", RunExportInput{Out: filepath.Join(other, "run.zip")}, outside)
	callErr(t, cs, "run.export", RunExportInput{Out: "run.zip", Policy: filepath.Join(other, "p.yaml")}, outside)
	callErr(t, cs, "world.write", WorldWriteInput{Path: "../x", Content: "x"}, "invalid path")
	callErr(t, cs, "world.write", WorldWriteInput{Path: "/abs", Content: "x"}, "invalid path")

	res := rawCall(t, cs, "replay.verify", ReplayVerifyInput{Bundle: filepath.Join(other, "run.zip")})
	if !res.IsError || !strings.Contains(resultText(res), outside) {
		t.Fatalf("replay.verify outside root: %s", resultText(res))
	}
	if _, err := os.Stat(filepath.Join(other, "run.zip")); !os.IsNotExist(err) {
		t.Fatal("a bundle was written outside the root")
	}
}

func TestForkIntoSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "f.txt", Content: "x"})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})

	callErr(t, cs, "run.fork", RunForkInput{Step: 1, Into: "link"}, "symlink")
	callErr(t, cs, "run.fork", RunForkInput{Step: 1, Into: "link/sub"}, "symlink")
	if _, err := os.Stat(filepath.Join(target, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("fork wrote through a symlink")
	}
}

func TestSecretWithoutPolicyBlocksExport(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "k.txt", Content: "key=" + awsKey})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})

	callErr(t, cs, "run.export", RunExportInput{Out: "run.zip"}, "fail-closed")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("export left files behind: %v", entries)
	}
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "still-open"})

	writeFile(t, filepath.Join(root, "policy.yaml"), "rules:\n  - id: aws-access-token\n    action: redact\n")
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip", Policy: "policy.yaml"})
	b := importBundle(t, filepath.Join(root, "run.zip"))
	last := b.Recording.Steps[len(b.Recording.Steps)-1]
	data, err := vfs.Fork(b.Store, snapOf(t, last.Root)).ReadFile("k.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), awsKey) || !strings.Contains(string(data), "***") {
		t.Fatalf("secret was not redacted: %q", data)
	}
}

func TestWorldReadTruncationAndBase64(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 8)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "d/long.txt", Content: "0123456789abcdef"})
	bin := []byte{0xff, 0xfe, 0x00, 0x01}
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "bin.dat", Content: base64.StdEncoding.EncodeToString(bin), Encoding: "base64"})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})

	long := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "d/long.txt"})
	if long.Kind != "file" || !long.Truncated || long.Content != "01234567" || long.Encoding != "text" || long.Size != 16 {
		t.Fatalf("unexpected truncated read: %+v", long)
	}

	b := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "bin.dat"})
	got, err := base64.StdEncoding.DecodeString(b.Content)
	if err != nil || b.Encoding != "base64" || !reflect.DeepEqual(got, bin) || b.Truncated {
		t.Fatalf("unexpected binary read: %+v (%v)", b, err)
	}

	dir := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "."})
	var names []string
	for _, e := range dir.Entries {
		names = append(names, e.Name+":"+e.Kind)
	}
	sort.Strings(names)
	if dir.Kind != "dir" || !reflect.DeepEqual(names, []string{"bin.dat:file", "d:dir"}) {
		t.Fatalf("unexpected listing: %+v", dir)
	}

	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "d/long.txt", Delete: true})
	callErr(t, cs, "world.read", WorldReadInput{Path: "d/long.txt"}, "")
	step := 1
	old := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "d/long.txt", Step: &step})
	if old.Content != "01234567" {
		t.Fatalf("unexpected read at step: %+v", old)
	}
	missing := 9
	callErr(t, cs, "world.read", WorldReadInput{Path: ".", Step: &missing}, "out of range")
}

func TestWorldDiffMatchesVFSDiff(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "keep.txt", Content: "k"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "mod.txt", Content: "v1"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "gone/x.txt", Content: "g"})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "mod.txt", Content: "v2"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "new/y.txt", Content: "n"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "gone", Delete: true})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s2"})

	got := call[WorldDiffOutput](t, cs, "world.diff", WorldDiffInput{From: "1", To: "2"})
	if len(got.Changes) != 3 {
		t.Fatalf("unexpected changes: %+v", got.Changes)
	}

	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	b := importBundle(t, filepath.Join(root, "run.zip"))
	want, err := vfs.Diff(b.Store, snapOf(t, b.Recording.Steps[1].Root), snapOf(t, b.Recording.Steps[2].Root))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[vfs.ChangeKind]string{vfs.Added: "added", vfs.Modified: "modified", vfs.Deleted: "deleted"}
	var wantInfo []ChangeInfo
	for _, c := range want {
		ci := ChangeInfo{Path: c.Path, Kind: kinds[c.Kind]}
		if c.Kind != vfs.Added {
			ci.From = hex.EncodeToString(c.From[:])
		}
		if c.Kind != vfs.Deleted {
			ci.To = hex.EncodeToString(c.To[:])
		}
		wantInfo = append(wantInfo, ci)
	}
	sort.Slice(wantInfo, func(i, j int) bool { return wantInfo[i].Path < wantInfo[j].Path })
	if !reflect.DeepEqual(got.Changes, wantInfo) {
		t.Fatalf("diff mismatch:\n got %+v\nwant %+v", got.Changes, wantInfo)
	}

	same := call[WorldDiffOutput](t, cs, "world.diff", WorldDiffInput{From: "2", To: "live"})
	if len(same.Changes) != 0 || same.Changes == nil {
		t.Fatalf("expected empty change list, got %+v", same.Changes)
	}
	callErr(t, cs, "world.diff", WorldDiffInput{From: "x", To: "live"}, "invalid step")
	callErr(t, cs, "world.diff", WorldDiffInput{From: "0", To: "7"}, "out of range")
}

func TestForkContents(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "top.txt", Content: "top"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "dir/inner.txt", Content: "inner"})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})
	call[WorldWriteOutput](t, cs, "world.write", WorldWriteInput{Path: "later.txt", Content: "later"})
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s2"})

	out := call[RunForkOutput](t, cs, "run.fork", RunForkInput{Step: 1, Into: "forked"})
	if out.Name != "s1" || out.FilesWritten != 2 {
		t.Fatalf("unexpected fork output: %+v", out)
	}
	for path, want := range map[string]string{"top.txt": "top", "dir/inner.txt": "inner"} {
		data, err := os.ReadFile(filepath.Join(root, "forked", filepath.FromSlash(path))) //nolint:gosec
		if err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", path, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "forked", "later.txt")); !os.IsNotExist(err) {
		t.Fatal("step 1 fork contains a file written after it")
	}
	callErr(t, cs, "run.fork", RunForkInput{Step: 5, Into: "x"}, "out of range")
	callErr(t, cs, "run.fork", RunForkInput{Step: -1, Into: "x"}, "out of range")
}

func TestReplayVerify(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	_, s1Hash := exportTwoStepBundle(t, cs, root)

	plain := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip"})
	if !plain.Valid || plain.Status != "match" || len(plain.Steps) != 2 || plain.Complete || len(plain.Checks) != 0 {
		t.Fatalf("unexpected plain verify: %+v", plain)
	}

	ok := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: s1Hash}}})
	if ok.Status != "match" || !ok.Complete || len(ok.Checks) != 1 || !ok.Checks[0].Match || ok.Divergence != nil {
		t.Fatalf("unexpected match verify: %+v", ok)
	}

	bad := strings.Repeat("0", 64)
	world := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: bad}}})
	if world.Status != "diverged" || !world.Valid || world.Divergence == nil || world.Divergence.Class != "world-mismatch" || world.Divergence.Step != 1 || world.Checks[0].Match {
		t.Fatalf("unexpected world divergence: %+v", world)
	}

	name := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "other", WorldHash: s1Hash}}})
	if name.Status != "diverged" || name.Divergence.Class != "step-mismatch" {
		t.Fatalf("unexpected name divergence: %+v", name)
	}

	extra := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: s1Hash}, {Name: "s2", WorldHash: s1Hash}}})
	if extra.Status != "diverged" || extra.Divergence.Class != "exhausted" {
		t.Fatalf("unexpected exhausted divergence: %+v", extra)
	}

	callErr(t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: "zz"}}}, "invalid world hash")

	data, err := os.ReadFile(filepath.Join(root, "run.zip")) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "cut.zip"), string(data[:len(data)/2]))
	cut := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "cut.zip", Steps: []StepHash{{Name: "s1", WorldHash: s1Hash}}})
	if cut.Valid || cut.Status != "invalid" || cut.Message == "" || cut.Divergence != nil {
		t.Fatalf("unexpected corrupt verify: %+v", cut)
	}
	writeFile(t, filepath.Join(root, "junk.zip"), "not a zip")
	junk := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "junk.zip"})
	if junk.Valid || junk.Status != "invalid" {
		t.Fatalf("unexpected junk verify: %+v", junk)
	}
	missing := call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "absent.zip"})
	if missing.Valid || missing.Status != "invalid" {
		t.Fatalf("unexpected missing verify: %+v", missing)
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %d", p, info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplayVerifyCreatesNoFiles(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	_, s1Hash := exportTwoStepBundle(t, cs, root)
	writeFile(t, filepath.Join(root, "junk.zip"), "not a zip")

	before := listTree(t, root)
	call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: s1Hash}}})
	call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "run.zip", Steps: []StepHash{{Name: "s1", WorldHash: strings.Repeat("1", 64)}}})
	call[ReplayVerifyOutput](t, cs, "replay.verify", ReplayVerifyInput{Bundle: "junk.zip"})
	after := listTree(t, root)

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("replay.verify changed the filesystem:\nbefore %v\nafter  %v", before, after)
	}
}

func TestToolsListIsExactlyTheSRSSet(t *testing.T) {
	cs := newSession(t, t.TempDir(), 0)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := []string{"replay.verify", "run.effect", "run.export", "run.fork", "run.input", "run.start", "run.step", "world.diff", "world.read", "world.write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestParallelEffects(t *testing.T) {
	root := t.TempDir()
	cs := newSession(t, root, 0)
	call[RunStartOutput](t, cs, "run.start", RunStartInput{})

	const n = 40
	var mu sync.Mutex
	seqs := map[int]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
				Name:      "run.effect",
				Arguments: RunEffectInput{Op: "tool:x", Key: fmt.Sprintf("k%d", i), Value: fmt.Sprintf("v%d", i)},
			})
			if err != nil || res.IsError {
				t.Errorf("effect %d failed: %v %v", i, err, res)
				return
			}
			b, _ := json.Marshal(res.StructuredContent)
			var out RunEffectOutput
			if err := json.Unmarshal(b, &out); err != nil {
				t.Errorf("effect %d: %v", i, err)
				return
			}
			mu.Lock()
			seqs[out.Seq] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(seqs) != n {
		t.Fatalf("expected %d distinct seq values, got %d", n, len(seqs))
	}
	for i := 0; i < n; i++ {
		if !seqs[i] {
			t.Fatalf("missing seq %d", i)
		}
	}
	call[RunStepOutput](t, cs, "run.step", RunStepInput{Name: "s1"})
	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	b := importBundle(t, filepath.Join(root, "run.zip"))
	if len(b.Recording.Effects) != n {
		t.Fatalf("expected %d effects, got %d", n, len(b.Recording.Effects))
	}
	for i, e := range b.Recording.Effects {
		if e.Seq != i || string(e.Value) != "v"+strings.TrimPrefix(e.Key, "k") {
			t.Fatalf("effect %d inconsistent: %+v", i, e)
		}
	}
}

func TestInputAndBase64Effect(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "repo", "main.go"), "package main")
	cs := newSession(t, root, 0)

	call[RunStartOutput](t, cs, "run.start", RunStartInput{})
	callErr(t, cs, "run.input", RunInputInput{Name: "empty"}, "required")
	in := call[RunInputOutput](t, cs, "run.input", RunInputInput{
		Name:    "checkout",
		FromDir: "repo",
		Files:   []InputFile{{Path: "extra/blob.bin", Content: base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), Encoding: "base64"}},
	})
	if in.N != 1 || len(in.WorldHash) != 64 {
		t.Fatalf("unexpected input output: %+v", in)
	}
	main := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "main.go"})
	blob := call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "extra/blob.bin"})
	if main.Content != "package main" || blob.Encoding != "text" || blob.Size != 3 {
		t.Fatalf("unexpected loaded world: %+v %+v", main, blob)
	}

	call[RunEffectOutput](t, cs, "run.effect", RunEffectInput{Op: "http", Key: "GET /", Value: base64.StdEncoding.EncodeToString([]byte{0xff, 0x00}), Encoding: "base64"})
	callErr(t, cs, "run.input", RunInputInput{Name: "bad", Files: []InputFile{{Path: "../x", Content: "x"}}}, "invalid path")
	callErr(t, cs, "run.step", RunStepInput{Name: "after-poison"}, "poisoned")
	call[WorldReadOutput](t, cs, "world.read", WorldReadInput{Path: "."})

	call[RunExportOutput](t, cs, "run.export", RunExportInput{Out: "run.zip"})
	b := importBundle(t, filepath.Join(root, "run.zip"))
	if b.Recording.Meta["incomplete"] != "true" || !reflect.DeepEqual(b.Recording.Effects[0].Value, []byte{0xff, 0x00}) {
		t.Fatalf("unexpected bundle: %+v", b.Recording)
	}
}
