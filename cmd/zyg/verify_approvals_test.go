package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const divMessage = `expected "other", got "plan"`

func approvalsPathIn(corpus string) string {
	return filepath.Join(corpus, ".zygote", "approvals.yaml")
}

func writeApprovalsFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func approvalYAML(entries ...string) string {
	return "version: 1\napprovals:\n" + strings.Join(entries, "")
}

func entryYAML(recording string, step int, class, reason string) string {
	return "  - recording: " + recording + "\n    step: " + strconv.Itoa(step) + "\n    class: " + class + "\n    reason: " + reason + "\n"
}

func readFileString(t testing.TB, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestVerifyApprovedDivergencePasses(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div})
	writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "new flow")))

	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
	if res.code != 0 || rep.Summary.Approved != 1 || rep.Summary.Diverged != 0 || rep.Summary.Total != 1 {
		t.Fatalf("exit %d: %+v", res.code, rep.Summary)
	}
	r := rep.Recordings[0]
	if r.Verdict != "approved" || r.ApprovalReason != "new flow" || r.Class != "step-mismatch" || r.Step == nil || *r.Step != 1 || r.Message != divMessage {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !strings.Contains(res.stdout, `"approval_reason": "new flow"`) || len(rep.Warnings) != 0 || !strings.Contains(res.stdout, `"warnings": []`) {
		t.Fatalf("json: %s", res.stdout)
	}

	human := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent)
	want := "zyg verify: 1 recordings\n" +
		"  APPROVED  a.zip  [step-mismatch] step 1: " + divMessage + "  (approved: new flow)\n" +
		"0 passed, 1 approved, 0 diverged, 0 invalid, 0 error  (exit 0)\n"
	if human.code != 0 || human.stdout != want {
		t.Fatalf("exit %d, stdout %q, want %q", human.code, human.stdout, want)
	}
}

func TestVerifyApprovalExactMatchOnly(t *testing.T) {
	f := newVerifyFixture(t)
	tests := []struct {
		name  string
		entry string
		stale string
	}{
		{"wrong step", entryYAML("a.zip", 2, "step-mismatch", "r"), "warning: stale approval a.zip step 2 [step-mismatch]: recording no longer diverges this way"},
		{"wrong class", entryYAML("a.zip", 1, "effect-mismatch", "r"), "warning: stale approval a.zip step 1 [effect-mismatch]: recording no longer diverges this way"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := makeCorpus(t, map[string]string{"a.zip": f.div})
			writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(tt.entry))

			rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
			if res.code != 1 || rep.Recordings[0].Verdict != "diverged" || rep.Summary.Approved != 0 {
				t.Fatalf("exit %d: %+v", res.code, rep)
			}
			if len(rep.Warnings) != 1 || rep.Warnings[0].Problem != "no-longer-applies" || rep.Warnings[0].Reason != "r" || rep.Warnings[0].Recording != "a.zip" {
				t.Fatalf("warnings: %+v", rep.Warnings)
			}

			human := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent)
			if human.code != 1 || !strings.Contains(human.stderr, tt.stale) {
				t.Fatalf("exit %d, stderr %q", human.code, human.stderr)
			}
		})
	}
}

func TestVerifyApprovalMixedCorpus(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)

	t.Run("approved and unapproved divergence", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.eff})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "r")))
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 1 || rep.Summary.Approved != 1 || rep.Summary.Diverged != 1 {
			t.Fatalf("exit %d: %+v", res.code, rep.Summary)
		}
		if findResult(t, rep, "a.zip").Verdict != "approved" || findResult(t, rep, "b.zip").Verdict != "diverged" {
			t.Fatalf("verdicts: %+v", rep.Recordings)
		}
	})
	t.Run("approved and invalid", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "bad.zip": garbage})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "r")))
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 2 || rep.Summary.Approved != 1 || rep.Summary.Invalid != 1 {
			t.Fatalf("exit %d: %+v", res.code, rep.Summary)
		}
	})
}

func TestVerifyApprovalNeverHidesInvalidOrError(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)

	t.Run("invalid", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"bad.zip": garbage})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("bad.zip", 1, "step-mismatch", "r")))
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 2 || rep.Recordings[0].Verdict != "invalid" || rep.Summary.Approved != 0 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
		if len(rep.Warnings) != 1 || rep.Warnings[0].Problem != "no-longer-applies" {
			t.Fatalf("warnings: %+v", rep.Warnings)
		}
	})
	t.Run("error", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"err.zip": f.errB})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("err.zip", 1, "step-mismatch", "r")))
		rep, res := verifyRun(t, []string{"ZYG_TEST_FAIL=1"}, "--corpus", corpus, "--agent", f.agent)
		if res.code != 3 || rep.Recordings[0].Verdict != "error" || rep.Summary.Approved != 0 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
	})
}

func TestVerifyStaleApprovals(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"ok.zip": f.ok})
	writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(
		entryYAML("gone.zip", 4, "effect-mismatch", "removed run"),
		entryYAML("ok.zip", 1, "step-mismatch", "fixed since"),
	))

	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
	if res.code != 0 || rep.Summary.Matched != 1 || len(rep.Warnings) != 2 {
		t.Fatalf("exit %d: %+v", res.code, rep)
	}
	w := rep.Warnings
	if w[0].Recording != "gone.zip" || w[0].Problem != "missing" || w[0].Reason != "removed run" || w[0].Class != "effect-mismatch" || w[0].Step == nil || *w[0].Step != 4 {
		t.Fatalf("warning 0: %+v", w[0])
	}
	if w[1].Recording != "ok.zip" || w[1].Problem != "matches" {
		t.Fatalf("warning 1: %+v", w[1])
	}

	human := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent)
	if human.code != 0 {
		t.Fatalf("exit %d", human.code)
	}
	for _, want := range []string{
		"warning: stale approval gone.zip step 4 [effect-mismatch]: recording is not in the corpus",
		"warning: stale approval ok.zip step 1 [step-mismatch]: recording now matches",
	} {
		if !strings.Contains(human.stderr, want) {
			t.Fatalf("stderr missing %q: %s", want, human.stderr)
		}
	}
	if !strings.Contains(human.stdout, "1 passed, 0 approved, 0 diverged, 0 invalid, 0 error  (exit 0)") {
		t.Fatalf("stdout: %s", human.stdout)
	}
}

func TestVerifyInvalidApprovalsFile(t *testing.T) {
	f := newVerifyFixture(t)
	valid := entryYAML("a.zip", 1, "step-mismatch", "r")

	tests := []struct {
		name    string
		content string
		dir     bool
		want    string
	}{
		{"bad yaml", "version: [1\napprovals: x", false, "approvals file"},
		{"unknown field", "version: 1\napprovals:\n  - recording: a.zip\n    step: 1\n    class: step-mismatch\n    reason: r\n    owner: me\n", false, "owner"},
		{"unknown top-level field", "version: 1\nextra: true\napprovals: []\n", false, "extra"},
		{"version 2", "version: 2\napprovals: []\n", false, "unsupported version 2"},
		{"missing version", "approvals: []\n", false, "unsupported version 0"},
		{"empty file", "", false, "file is empty"},
		{"empty recording", approvalYAML(entryYAML("\"\"", 1, "step-mismatch", "r")), false, "recording is required"},
		{"empty class", approvalYAML(entryYAML("a.zip", 1, "\"\"", "r")), false, "class is required"},
		{"empty reason", approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "\"\"")), false, "reason is required"},
		{"unknown class", approvalYAML(entryYAML("a.zip", 1, "made-up", "r")), false, `unknown class "made-up"`},
		{"duplicate", approvalYAML(valid, valid), false, "duplicate approval"},
		{"is a directory", "", true, "approvals file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := makeCorpus(t, map[string]string{"a.zip": f.div})
			path := approvalsPathIn(corpus)
			if tt.dir {
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
			} else {
				writeApprovalsFile(t, path, tt.content)
			}
			marker := filepath.Join(t.TempDir(), "agent-ran")

			res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", "touch '"+filepath.ToSlash(marker)+"'")
			if res.code != 3 {
				t.Fatalf("exit %d, want 3\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("stderr %q should name %q and contain %q", res.stderr, path, tt.want)
			}
			if res.stdout != "" || fileExists(marker) {
				t.Fatalf("nothing may be replayed: stdout %q, agent ran %v", res.stdout, fileExists(marker))
			}
		})
	}
}

func TestVerifyNoApprovalsFileUnchanged(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.ok})

	res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent)
	want := "zyg verify: 2 recordings\n" +
		"  DIVERGED  a.zip  [step-mismatch] step 1: " + divMessage + "\n" +
		"  PASS      b.zip\n" +
		"1 passed, 1 diverged, 0 invalid, 0 error  (exit 1)\n"
	if res.code != 1 || res.stdout != want || res.stderr != "" {
		t.Fatalf("exit %d\nstdout %q\nwant   %q\nstderr %q", res.code, res.stdout, want, res.stderr)
	}
	if fileExists(filepath.Join(corpus, ".zygote")) {
		t.Fatal("verify must not create .zygote without --approve")
	}

	rep, jres := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
	if jres.code != 1 || rep.Summary.Approved != 0 || rep.Summary.Diverged != 1 || rep.Summary.Matched != 1 || len(rep.Warnings) != 0 {
		t.Fatalf("exit %d: %+v", jres.code, rep)
	}
	if strings.Contains(jres.stdout, "approval_reason") || !strings.Contains(jres.stdout, `"warnings": []`) {
		t.Fatalf("json: %s", jres.stdout)
	}
}

func TestVerifyApproveWritesFile(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.eff, "c.zip": f.ok})
	path := approvalsPathIn(corpus)

	res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approve", "a.zip", "--reason", "first")
	if res.code != 1 || !strings.Contains(res.stdout, "  APPROVED  a.zip") || !strings.Contains(res.stdout, "  DIVERGED  b.zip") {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	want := approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "first"))
	if got := readFileString(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}

	if res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approve", "b.zip", "--reason", "second"); res.code != 0 {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	want = approvalYAML(
		entryYAML("a.zip", 1, "step-mismatch", "first"),
		entryYAML("b.zip", 0, "effect-mismatch", "second"),
	)
	if got := readFileString(t, path); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}

	if _, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent); res.code != 0 {
		t.Fatalf("second run exit %d: %s", res.code, res.stdout)
	}

	if res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approve", "a.zip", "--reason", "updated"); res.code != 0 {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	want = approvalYAML(
		entryYAML("a.zip", 1, "step-mismatch", "updated"),
		entryYAML("b.zip", 0, "effect-mismatch", "second"),
	)
	got := readFileString(t, path)
	if got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}

	runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approve", "a.zip", "--reason", "updated")
	if again := readFileString(t, path); again != got {
		t.Fatalf("output is not byte-identical:\n%s\nvs\n%s", again, got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("leftover temp files: %v %v", entries, err)
	}
}

func TestVerifyApproveSortsEntries(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"z.zip": f.div, "a.zip": f.div})

	res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approve", "z.zip", "--approve", "a.zip", "--reason", "both")
	if res.code != 0 {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	want := approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "both"), entryYAML("z.zip", 1, "step-mismatch", "both"))
	if got := readFileString(t, approvalsPathIn(corpus)); got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestVerifyApproveErrors(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)
	seed := approvalYAML(entryYAML("keep.zip", 1, "step-mismatch", "keep"))

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing reason", []string{"--approve", "a.zip"}, "--reason is required"},
		{"empty reason", []string{"--approve", "a.zip", "--reason", ""}, "--reason is required"},
		{"blank reason", []string{"--approve", "a.zip", "--reason", "  "}, "--reason is required"},
		{"unknown recording", []string{"--approve", "nope.zip", "--reason", "r"}, "recording not found in corpus"},
		{"matched recording", []string{"--approve", "ok.zip", "--reason", "r"}, "only a diverged recording can be approved"},
		{"invalid recording", []string{"--approve", "bad.zip", "--reason", "r"}, "only a diverged recording can be approved"},
		{"one bad name blocks all", []string{"--approve", "a.zip", "--approve", "ok.zip", "--reason", "r"}, "only a diverged recording can be approved"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" without file", func(t *testing.T) {
			corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "ok.zip": f.ok, "bad.zip": garbage})
			res := runZygSplit(t, nil, append([]string{"verify", "--corpus", corpus, "--agent", f.agent}, tt.args...)...)
			if res.code != 3 || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
			}
			if fileExists(filepath.Join(corpus, ".zygote")) {
				t.Fatal("approvals file or directory was created")
			}
		})
		t.Run(tt.name+" with file", func(t *testing.T) {
			corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "ok.zip": f.ok, "bad.zip": garbage})
			writeApprovalsFile(t, approvalsPathIn(corpus), seed)
			res := runZygSplit(t, nil, append([]string{"verify", "--corpus", corpus, "--agent", f.agent}, tt.args...)...)
			if res.code != 3 {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr)
			}
			if got := readFileString(t, approvalsPathIn(corpus)); got != seed {
				t.Fatalf("file modified:\n%s", got)
			}
		})
	}
}

func TestVerifyApprovalsFlagOverride(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div})
	custom := filepath.Join(t.TempDir(), "nested", "my-approvals.yaml")

	res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent, "--approvals", custom, "--approve", "a.zip", "--reason", "custom")
	if res.code != 0 {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	if want := approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "custom")); readFileString(t, custom) != want {
		t.Fatalf("custom file:\n%s", readFileString(t, custom))
	}
	if fileExists(filepath.Join(corpus, ".zygote")) {
		t.Fatal("default approvals location must not be used")
	}

	if _, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent, "--approvals", custom); res.code != 0 {
		t.Fatalf("with --approvals: exit %d", res.code)
	}
	if _, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent); res.code != 1 {
		t.Fatalf("without --approvals: exit %d, want 1", res.code)
	}
}

func TestVerifyApprovedEncryptedCorpus(t *testing.T) {
	requireShell(t)
	id, idFile := writeIdentity(t)
	agent := fakeAgent(t, "effect|model|mode|"+b64Ok, "step|plan")
	enc := recordFake(t, nil, fakeAgent(t, "effect|model|mode|"+b64Ok, "step|other"), "--encrypt", "--recipient", id.Recipient().String())
	corpus := makeCorpus(t, map[string]string{"enc.zip": enc})

	if res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", agent, "--approve", "enc.zip", "--reason", "r"); res.code != 3 {
		t.Fatalf("approve without key: exit %d (an undecryptable recording is not diverged)", res.code)
	}
	if fileExists(approvalsPathIn(corpus)) {
		t.Fatal("approvals written for an undecryptable recording")
	}

	res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", agent, "--identity-file", idFile, "--approve", "enc.zip", "--reason", "encrypted")
	if res.code != 0 || !strings.Contains(res.stdout, "  APPROVED  enc.zip") {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}
	rep, jres := verifyRun(t, nil, "--corpus", corpus, "--agent", agent, "--identity-file", idFile)
	if jres.code != 0 || rep.Summary.Approved != 1 || rep.Recordings[0].ApprovalReason != "encrypted" {
		t.Fatalf("exit %d: %+v", jres.code, rep)
	}
	if _, nokey := verifyRun(t, nil, "--corpus", corpus, "--agent", agent); nokey.code != 2 {
		t.Fatalf("without key: exit %d, want 2", nokey.code)
	}
}
