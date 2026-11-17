package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type verifyFixture struct {
	agent string
	ok    string
	div   string
	errB  string
	eff   string
}

func newVerifyFixture(t *testing.T) verifyFixture {
	t.Helper()
	requireShell(t)
	okOps := []string{"effect|model|mode|" + b64Ok, "failif|" + b64Err, "step|plan"}
	return verifyFixture{
		agent: fakeAgent(t, okOps...),
		ok:    recordFake(t, nil, fakeAgent(t, okOps...)),
		div:   recordFake(t, nil, fakeAgent(t, "effect|model|mode|"+b64Ok, "step|other")),
		errB:  recordFake(t, nil, fakeAgent(t, "effect|model|mode|"+b64Err, "failif|"+b64Err, "step|plan")),
		eff:   recordFake(t, nil, fakeAgent(t, "effect|model|other|"+b64Ok, "step|plan")),
	}
}

func verifyRun(t testing.TB, env []string, args ...string) (verifyReport, zygSplit) {
	t.Helper()
	res := runZygSplit(t, env, append([]string{"verify", "--json"}, args...)...)
	var rep verifyReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("decoding report: %v\nexit %d\nstdout: %s\nstderr: %s", err, res.code, res.stdout, res.stderr)
	}
	if rep.ExitCode != res.code {
		t.Fatalf("report exit_code %d, process exit %d", rep.ExitCode, res.code)
	}
	return rep, res
}

func findResult(t *testing.T, rep verifyReport, bundle string) recordingResult {
	t.Helper()
	for _, r := range rep.Recordings {
		if r.Bundle == bundle {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", bundle, rep.Recordings)
	return recordingResult{}
}

func TestVerifyExitCodes(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)

	tests := []struct {
		name     string
		bundle   string
		agent    string
		env      []string
		wantCode int
		wantText string
	}{
		{"all matched", f.ok, f.agent, nil, 0, "1 passed, 0 diverged, 0 invalid, 0 error  (exit 0)"},
		{"divergence", f.div, f.agent, nil, 1, "DIVERGED  b.zip  [step-mismatch] step 1:"},
		{"invalid bundle", garbage, f.agent, nil, 2, "INVALID   b.zip  zip: not a valid zip file"},
		{"agent fails", f.errB, f.agent, []string{"ZYG_TEST_FAIL=1"}, 3, "ERROR     b.zip  agent exited with error: exit status 4"},
		{"agent command missing", f.ok, "definitely-not-a-command-zyg", nil, 3, "ERROR     b.zip  agent exited with error: exit status 127"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := makeCorpus(t, map[string]string{"b.zip": tt.bundle})
			res := runZygSplit(t, tt.env, "verify", "--corpus", corpus, "--agent", tt.agent)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stdout, tt.wantText) {
				t.Fatalf("stdout missing %q:\n%s", tt.wantText, res.stdout)
			}
		})
	}
}

func TestVerifyExitCodePrecedence(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)
	pool := map[string]string{"ok": f.ok, "div": f.div, "err": f.errB, "inv": garbage}

	tests := []struct {
		members  []string
		wantCode int
	}{
		{[]string{"ok"}, 0},
		{[]string{"ok", "div"}, 1},
		{[]string{"ok", "inv"}, 2},
		{[]string{"ok", "err"}, 3},
		{[]string{"div", "inv"}, 2},
		{[]string{"div", "err"}, 3},
		{[]string{"inv", "err"}, 3},
		{[]string{"ok", "div", "inv", "err"}, 3},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.members, "+"), func(t *testing.T) {
			files := map[string]string{}
			for _, m := range tt.members {
				files[m+".zip"] = pool[m]
			}
			rep, res := verifyRun(t, []string{"ZYG_TEST_FAIL=1"}, "--corpus", makeCorpus(t, files), "--agent", f.agent)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d: %s", res.code, tt.wantCode, res.stdout)
			}
			if rep.Summary.Total != len(tt.members) {
				t.Fatalf("total %d, want %d", rep.Summary.Total, len(tt.members))
			}
		})
	}
}

func TestVerifyJSONSchema(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"b.zip": f.ok, "a.zip": f.div, "C.ZIP": f.eff})
	res := runZygSplit(t, nil, "verify", "--json", "--corpus", corpus, "--agent", f.agent)
	if res.code != 1 {
		t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "report", raw, "schema_version", "corpus", "summary", "exit_code", "recordings", "warnings")
	assertKeys(t, "summary", raw["summary"].(map[string]any), "total", "matched", "approved", "diverged", "invalid", "error")
	if raw["schema_version"] != float64(1) || raw["exit_code"] != float64(1) || raw["corpus"] != corpus {
		t.Fatalf("unexpected header: %v", raw)
	}

	recs := raw["recordings"].([]any)
	var names []string
	byName := map[string]map[string]any{}
	for _, r := range recs {
		m := r.(map[string]any)
		name := m["bundle"].(string)
		names = append(names, name)
		byName[name] = m
	}
	if want := []string{"C.ZIP", "a.zip", "b.zip"}; !slices.Equal(names, want) {
		t.Fatalf("order %v, want %v", names, want)
	}
	assertKeys(t, "match", byName["b.zip"], "bundle", "verdict", "duration_ms")
	assertKeys(t, "step-mismatch", byName["a.zip"], "bundle", "verdict", "class", "step", "step_name", "message", "duration_ms")
	assertKeys(t, "effect-mismatch", byName["C.ZIP"], "bundle", "verdict", "class", "step", "step_name", "message", "duration_ms")

	if byName["b.zip"]["verdict"] != "match" || byName["a.zip"]["verdict"] != "diverged" {
		t.Fatalf("unexpected verdicts: %v", byName)
	}
	if byName["a.zip"]["class"] != "step-mismatch" || byName["a.zip"]["step"] != float64(1) {
		t.Fatalf("step-mismatch result: %v", byName["a.zip"])
	}
	if byName["C.ZIP"]["class"] != "effect-mismatch" || byName["C.ZIP"]["step"] != float64(1) {
		t.Fatalf("effect divergence is reported at the step whose snapshot follows it: %v", byName["C.ZIP"])
	}

	var rep verifyReport
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatal(err)
	}
	want := verifySummary{Total: 3, Matched: 1, Diverged: 2}
	if rep.Summary != want {
		t.Fatalf("summary %+v, want %+v", rep.Summary, want)
	}
}

func assertKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s keys %v, want %v", what, got, want)
	}
}

func TestVerifyHumanSummary(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)
	corpus := makeCorpus(t, map[string]string{"1-ok.zip": f.ok, "2-div.zip": f.div, "3-bad.zip": garbage, "4-err.zip": f.errB})

	res := runZygSplit(t, []string{"ZYG_TEST_FAIL=1"}, "verify", "--corpus", corpus, "--agent", f.agent)
	if res.code != 3 {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	wantPrefixes := []string{
		"zyg verify: 4 recordings",
		"  PASS      1-ok.zip",
		"  DIVERGED  2-div.zip  [step-mismatch] step 1: ",
		"  INVALID   3-bad.zip  ",
		"  ERROR     4-err.zip  ",
		"1 passed, 1 diverged, 1 invalid, 1 error  (exit 3)",
	}
	if len(lines) != len(wantPrefixes) {
		t.Fatalf("got %d lines:\n%s", len(lines), res.stdout)
	}
	for i, p := range wantPrefixes {
		if !strings.HasPrefix(lines[i], p) {
			t.Fatalf("line %d %q, want prefix %q", i, lines[i], p)
		}
	}
}

func TestVerifyEncryptedCorpus(t *testing.T) {
	requireShell(t)
	id, idFile := writeIdentity(t)
	_, wrongFile := writeIdentity(t)
	ops := []string{"effect|model|mode|" + b64Ok, "step|plan"}
	agent := fakeAgent(t, ops...)
	enc := recordFake(t, nil, fakeAgent(t, ops...), "--encrypt", "--recipient", id.Recipient().String())
	plain := recordFake(t, nil, fakeAgent(t, ops...))

	t.Run("no key", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc}), "--agent", agent)
		if res.code != 2 || rep.Recordings[0].Verdict != "invalid" || !strings.Contains(rep.Recordings[0].Message, "bundle is encrypted") {
			t.Fatalf("exit %d: %+v", res.code, rep.Recordings)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc}), "--agent", agent, "--identity-file", wrongFile)
		if res.code != 2 || !strings.Contains(rep.Recordings[0].Message, "cannot decrypt bundle") {
			t.Fatalf("exit %d: %+v", res.code, rep.Recordings)
		}
	})
	t.Run("right key", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc}), "--agent", agent, "--identity-file", idFile)
		if res.code != 0 || rep.Summary.Matched != 1 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
	})
	t.Run("right key from env", func(t *testing.T) {
		_, res := verifyRun(t, []string{"ZYG_IDENTITY_FILE=" + idFile}, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc}), "--agent", agent)
		if res.code != 0 {
			t.Fatalf("exit %d: %s", res.code, res.stdout)
		}
	})
	t.Run("mixed with key", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc, "p.zip": plain}), "--agent", agent, "--identity-file", idFile)
		if res.code != 0 || rep.Summary.Matched != 2 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
	})
	t.Run("mixed without key", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": enc, "p.zip": plain}), "--agent", agent)
		if res.code != 2 || rep.Summary.Matched != 1 || rep.Summary.Invalid != 1 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
		if findResult(t, rep, "p.zip").Verdict != "match" || findResult(t, rep, "e.zip").Verdict != "invalid" {
			t.Fatalf("per-recording verdicts: %+v", rep.Recordings)
		}
	})
	t.Run("passphrase", func(t *testing.T) {
		passFile := writePassphraseFile(t, "correct horse battery")
		penc := recordFake(t, []string{"ZYG_PASSPHRASE_FILE=" + passFile}, fakeAgent(t, ops...), "--encrypt", "--passphrase")
		corpus := makeCorpus(t, map[string]string{"p.zip": penc})
		if _, res := verifyRun(t, nil, "--corpus", corpus, "--agent", agent); res.code != 2 {
			t.Fatalf("no passphrase: exit %d", res.code)
		}
		if _, res := verifyRun(t, []string{"ZYG_PASSPHRASE_FILE=" + passFile}, "--corpus", corpus, "--agent", agent); res.code != 0 {
			t.Fatalf("passphrase file: exit %d", res.code)
		}
	})
}

func TestKeysHiddenFromReplayAgent(t *testing.T) {
	requireShell(t)
	ops := []string{"effect|model|mode|" + b64Ok, "step|plan"}
	bundle := recordFake(t, nil, fakeAgent(t, ops...))
	agent := `[ -z "$ZYG_PASSPHRASE$ZYG_PASSPHRASE_FILE$ZYG_IDENTITY_FILE" ] || exit 9; ` + fakeAgent(t, ops...)
	_, idFile := writeIdentity(t)
	env := []string{"ZYG_PASSPHRASE=s3cret", "ZYG_PASSPHRASE_FILE=" + writePassphraseFile(t, "s3cret"), "ZYG_IDENTITY_FILE=" + idFile}

	if res := runZygSplit(t, env, "replay", bundle, "--agent", agent); res.code != 0 {
		t.Fatalf("replay: agent saw key variables (exit %d): %s%s", res.code, res.stdout, res.stderr)
	}
	corpus := makeCorpus(t, map[string]string{"a.zip": bundle})
	if _, res := verifyRun(t, env, "--corpus", corpus, "--agent", agent); res.code != 0 {
		t.Fatalf("verify: agent saw key variables (exit %d): %s", res.code, res.stdout)
	}
}

func TestVerifyAutoStepCorpus(t *testing.T) {
	requireShell(t)
	ops := []string{"effect|tool:search|q|" + b64Ok, "effect|model|m|" + b64Ok}
	bundle := recordFake(t, nil, fakeAgent(t, ops...), "--auto-step")
	corpus := makeCorpus(t, map[string]string{"auto.zip": bundle})

	if rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", fakeAgent(t, ops...)); res.code != 0 || rep.Summary.Matched != 1 {
		t.Fatalf("exit %d: %+v", res.code, rep)
	}

	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", "true")
	r := rep.Recordings[0]
	if res.code != 1 || r.Class != "step-mismatch" || r.Message != "agent stopped after step 0 of 2" {
		t.Fatalf("exit %d: %+v", res.code, r)
	}

	rep, res = verifyRun(t, nil, "--corpus", corpus, "--agent", fakeAgent(t, "effect|tool:search|q|"+b64Ok))
	if res.code != 1 || rep.Recordings[0].Class != "step-mismatch" {
		t.Fatalf("skipped final step: exit %d: %+v", res.code, rep.Recordings)
	}
}

func TestVerifyCASCorpus(t *testing.T) {
	requireShell(t)
	ops := []string{"big|model|k", "step|plan"}
	bundle := recordFake(t, nil, fakeAgent(t, ops...))

	zr, err := zip.OpenReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close() //nolint:errcheck
	spilled := false
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "objects/") && f.UncompressedSize64 == 40000 {
			spilled = true
		}
	}
	if !spilled {
		t.Fatal("effect value was not spilled to the object store")
	}

	corpus := makeCorpus(t, map[string]string{"cas.zip": bundle})
	if rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", fakeAgent(t, ops...)); res.code != 0 || rep.Summary.Matched != 1 {
		t.Fatalf("exit %d: %+v", res.code, rep)
	}
}

func TestVerifyAgentStoppedEarly(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.ok})

	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", "true")
	r := rep.Recordings[0]
	if res.code != 1 || r.Verdict != "diverged" || r.Class != "step-mismatch" || r.Step == nil || *r.Step != 1 || r.Message != "agent stopped after step 0 of 1" {
		t.Fatalf("exit %d: %+v", res.code, r)
	}
}

func TestVerifyCorpusEdgeCases(t *testing.T) {
	f := newVerifyFixture(t)

	t.Run("empty corpus", func(t *testing.T) {
		res := runZygSplit(t, nil, "verify", "--corpus", t.TempDir(), "--agent", f.agent)
		if res.code != 2 || !strings.Contains(res.stderr, "no bundles found") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		rep, jres := verifyRun(t, nil, "--corpus", t.TempDir(), "--agent", f.agent)
		if jres.code != 2 || rep.Recordings == nil || len(rep.Recordings) != 0 || !strings.Contains(jres.stdout, `"recordings": []`) {
			t.Fatalf("json: exit %d: %s", jres.code, jres.stdout)
		}
	})
	t.Run("missing corpus directory", func(t *testing.T) {
		res := runZygSplit(t, nil, "verify", "--corpus", filepath.Join(t.TempDir(), "nope"), "--agent", f.agent)
		if res.code != 3 {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})
	t.Run("corpus is a file", func(t *testing.T) {
		res := runZygSplit(t, nil, "verify", "--corpus", f.ok, "--agent", f.agent)
		if res.code != 3 || !strings.Contains(res.stderr, "not a directory") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})
	t.Run("missing agent", func(t *testing.T) {
		res := runZygSplit(t, nil, "verify", "--corpus", makeCorpus(t, map[string]string{"a.zip": f.ok}))
		if res.code != 3 || !strings.Contains(res.stderr, "--agent is required") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})
	t.Run("missing corpus flag", func(t *testing.T) {
		res := runZygSplit(t, nil, "verify", "--agent", f.agent)
		if res.code != 3 || !strings.Contains(res.stderr, "--corpus is required") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	})
	t.Run("non-zip files ignored", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.ok})
		for _, name := range []string{"notes.txt", "run.zip.bak", "README"} {
			if err := os.WriteFile(filepath.Join(corpus, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 0 || rep.Summary.Total != 1 {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
	})
	t.Run("sub-directories skipped", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.ok})
		for _, dir := range []string{"nested", "looks-like.zip"} {
			if err := os.Mkdir(filepath.Join(corpus, dir), 0o750); err != nil {
				t.Fatal(err)
			}
			copyFile(t, f.div, filepath.Join(corpus, dir, "inner.zip"))
		}
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 0 || rep.Summary.Total != 1 || rep.Recordings[0].Bundle != "a.zip" {
			t.Fatalf("exit %d: %+v", res.code, rep)
		}
	})
	t.Run("only sub-directories is empty", func(t *testing.T) {
		corpus := t.TempDir()
		if err := os.Mkdir(filepath.Join(corpus, "x.zip"), 0o750); err != nil {
			t.Fatal(err)
		}
		if res := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent); res.code != 2 {
			t.Fatalf("exit %d", res.code)
		}
	})
	t.Run("corrupt zip", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"good.zip": f.ok})
		writeGarbage(t, filepath.Join(corpus, "bad.zip"))
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 2 || findResult(t, rep, "bad.zip").Verdict != "invalid" || findResult(t, rep, "good.zip").Verdict != "match" {
			t.Fatalf("exit %d: %+v", res.code, rep.Recordings)
		}
	})
	t.Run("truncated zip", func(t *testing.T) {
		data, err := os.ReadFile(f.ok) //nolint:gosec
		if err != nil {
			t.Fatal(err)
		}
		corpus := t.TempDir()
		if err := os.WriteFile(filepath.Join(corpus, "cut.zip"), data[:len(data)/2], 0o600); err != nil { //nolint:gosec
			t.Fatal(err)
		}
		rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 2 || rep.Recordings[0].Verdict != "invalid" {
			t.Fatalf("exit %d: %+v", res.code, rep.Recordings)
		}
	})
}

func TestVerifyTimeout(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"slow.zip": f.ok})

	start := time.Now()
	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", "sleep 20 2>/dev/null", "--timeout", "1s")
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	r := rep.Recordings[0]
	if res.code != 3 || r.Verdict != "error" || r.Message != "timed out" {
		t.Fatalf("exit %d: %+v", res.code, r)
	}
}

func TestVerifyFastAgentNeverFlakes(t *testing.T) {
	requireShell(t)
	ops := []string{"step|plan"}
	bundle := recordFake(t, nil, fakeAgent(t, ops...))
	files := map[string]string{}
	for i := range 60 {
		files["r"+strconv.Itoa(i)+".zip"] = bundle
	}
	rep, res := verifyRun(t, nil, "--corpus", makeCorpus(t, files), "--agent", fakeAgent(t, ops...))
	if res.code != 0 || rep.Summary.Matched != 60 || rep.Summary.Error != 0 {
		t.Fatalf("exit %d: %+v\n%s", res.code, rep.Summary, res.stderr)
	}
}

func fixtureCorpus(t testing.TB, n int) (corpus, agent string) {
	t.Helper()
	requireShell(t)
	ops := []string{"effect|model|mode|" + b64Ok, "step|plan", "effect|tool:search|q|" + b64Ok, "step|act"}
	bundle := recordFake(t, nil, fakeAgent(t, ops...))
	files := make(map[string]string, n)
	for i := range n {
		files["run-"+strconv.Itoa(i)+".zip"] = bundle
	}
	return makeCorpus(t, files), fakeAgent(t, ops...)
}

func TestVerify40RecordingsUnderTwoMinutes(t *testing.T) {
	corpus, agent := fixtureCorpus(t, 40)

	start := time.Now()
	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", agent)
	elapsed := time.Since(start)

	if res.code != 0 || rep.Summary.Matched != 40 {
		t.Fatalf("exit %d: %+v", res.code, rep.Summary)
	}
	if elapsed >= 2*time.Minute {
		t.Fatalf("verifying 40 recordings took %s, want under 2m (SRS 11.5)", elapsed)
	}
	t.Logf("verified 40 recordings in %s", elapsed)
}

func BenchmarkVerify40(b *testing.B) {
	corpus, agent := fixtureCorpus(b, 40)
	b.ResetTimer()
	for range b.N {
		if res := runZygSplit(b, nil, "verify", "--corpus", corpus, "--agent", agent); res.code != 0 {
			b.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
	}
}
