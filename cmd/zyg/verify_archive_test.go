package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const mergedAgentOps = "effect|model|mode|" + b64Ok

// mergedFixture returns a merged archive of runs r1..r3 plus the agent that recorded them.
func mergedFixture(t *testing.T) (archive, agent string) {
	t.Helper()
	dir := bundleDir(t, "r1.zip", "r2.zip", "r3.zip")
	return mergedArchive(t, dir, "r1.zip", "r2.zip", "r3.zip"), fakeAgent(t, mergedAgentOps, "step|plan")
}

func recordingNames(rep verifyReport) []string {
	var names []string
	for _, r := range rep.Recordings {
		names = append(names, r.Bundle)
	}
	return names
}

func TestVerifyMergedArchive(t *testing.T) {
	archive, agent := mergedFixture(t)

	t.Run("same agent passes", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", archive, "--agent", agent)
		if res.code != 0 || rep.Summary.Total != 3 || rep.Summary.Matched != 3 {
			t.Fatalf("exit %d: %+v", res.code, rep.Summary)
		}
		if got := recordingNames(rep); !slices.Equal(got, []string{"r1", "r2", "r3"}) {
			t.Fatalf("recordings %v", got)
		}
		human := runZygSplit(t, nil, "verify", "--corpus", archive, "--agent", agent)
		for _, n := range []string{"PASS      r1", "PASS      r2", "PASS      r3", "3 passed, 0 diverged, 0 invalid, 0 error  (exit 0)"} {
			if !strings.Contains(human.stdout, n) {
				t.Fatalf("stdout lacks %q:\n%s", n, human.stdout)
			}
		}
	})

	changed := fakeAgent(t, mergedAgentOps, "step|other")
	t.Run("changed agent diverges", func(t *testing.T) {
		rep, res := verifyRun(t, nil, "--corpus", archive, "--agent", changed)
		if res.code != 1 || rep.Summary.Diverged != 3 {
			t.Fatalf("exit %d: %+v", res.code, rep.Summary)
		}
		for _, name := range []string{"r1", "r2", "r3"} {
			if r := findResult(t, rep, name); r.Verdict != "diverged" || r.Class != "step-mismatch" || r.StepName != "plan" {
				t.Fatalf("%s: %+v", name, r)
			}
		}
	})

	t.Run("approvals by run name", func(t *testing.T) {
		path := filepath.Join(filepath.Dir(archive), ".zygote", "approvals.yaml")
		if fileExists(path) {
			t.Fatal("approvals file exists before approving")
		}
		res := runZygSplit(t, nil, "verify", "--corpus", archive, "--agent", changed, "--approve", "r1", "--approve", "r2", "--reason", "new flow")
		if res.code != 1 || !strings.Contains(res.stdout, "APPROVED  r1") || !strings.Contains(res.stdout, "DIVERGED  r3") {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		content := readFileString(t, path)
		if !strings.Contains(content, "recording: r1") || !strings.Contains(content, "recording: r2") || strings.Contains(content, "r3") {
			t.Fatalf("approvals file: %s", content)
		}
		if res := runZygSplit(t, nil, "verify", "--corpus", archive, "--agent", changed, "--approve", "r3", "--reason", "new flow"); res.code != 0 {
			t.Fatalf("exit %d: %s%s", res.code, res.stdout, res.stderr)
		}
		if res := runZygSplit(t, nil, "verify", "--corpus", archive, "--agent", changed, "--approve", "nope", "--reason", "x"); res.code != 3 || !strings.Contains(res.stderr, "recording not found in corpus") {
			t.Fatalf("unknown run: exit %d: %s", res.code, res.stderr)
		}
	})

	t.Run("corrupt archive is invalid", func(t *testing.T) {
		garbage := filepath.Join(t.TempDir(), "garbage.zip")
		writeGarbage(t, garbage)
		rep, res := verifyRun(t, nil, "--corpus", garbage, "--agent", agent)
		if res.code != 2 || rep.Summary.Invalid != 1 || !strings.Contains(rep.Recordings[0].Message, "zip: not a valid zip file") {
			t.Fatalf("garbage: exit %d: %+v", res.code, rep)
		}

		bad := filepath.Join(t.TempDir(), "bad.zip")
		corruptFirstObject(t, archive, bad)
		rep, res = verifyRun(t, nil, "--corpus", bad, "--agent", agent)
		if res.code != 2 || rep.Summary.Invalid != 1 || !strings.Contains(rep.Recordings[0].Message, "does not match its hash") {
			t.Fatalf("hash mismatch: exit %d: %+v", res.code, rep)
		}
	})
}

func TestVerifySingleBundleFileCorpus(t *testing.T) {
	requireShell(t)
	agent := fakeAgent(t, mergedAgentOps, "step|plan")
	dir := t.TempDir()
	bundle := filepath.Join(dir, "solo.zip")
	copyFile(t, recordFake(t, nil, agent), bundle)

	rep, res := verifyRun(t, nil, "--corpus", bundle, "--agent", agent)
	if res.code != 0 || rep.Summary.Total != 1 || rep.Recordings[0].Bundle != "solo.zip" {
		t.Fatalf("exit %d: %+v", res.code, rep)
	}

	changed := fakeAgent(t, mergedAgentOps, "step|other")
	approvals := filepath.Join(dir, ".zygote", "approvals.yaml")
	res = runZygSplit(t, nil, "verify", "--corpus", bundle, "--agent", changed, "--approve", "solo.zip", "--reason", "ok")
	if res.code != 0 || !fileExists(approvals) {
		t.Fatalf("exit %d, approvals at %s: %s%s", res.code, approvals, res.stdout, res.stderr)
	}
	if !strings.Contains(readFileString(t, approvals), "recording: solo.zip") {
		t.Fatal("approvals file does not name the bundle")
	}
}

func TestVerifyEncryptedMergedArchive(t *testing.T) {
	requireShell(t)
	id, idFile := writeIdentity(t)
	agent := fakeAgent(t, mergedAgentOps, "step|plan")
	dir := t.TempDir()
	for _, n := range []string{"e1.zip", "e2.zip"} {
		copyFile(t, recordFake(t, nil, agent, "--encrypt", "--recipient", id.Recipient().String()), filepath.Join(dir, n))
	}
	secret := filepath.Join(dir, "secret.zip")
	res := runZygSplit(t, nil, "merge", "-o", secret, "--identity-file", idFile, "--encrypt", "--recipient", id.Recipient().String(),
		filepath.Join(dir, "e1.zip"), filepath.Join(dir, "e2.zip"))
	if res.code != 0 {
		t.Fatalf("merge exit %d: %s%s", res.code, res.stdout, res.stderr)
	}

	rep, vres := verifyRun(t, nil, "--corpus", secret, "--agent", agent, "--identity-file", idFile)
	if vres.code != 0 || rep.Summary.Matched != 2 || !slices.Equal(recordingNames(rep), []string{"e1", "e2"}) {
		t.Fatalf("exit %d: %+v", vres.code, rep)
	}
	rep, vres = verifyRun(t, []string{"ZYG_IDENTITY_FILE=" + idFile}, "--corpus", secret, "--agent", agent)
	if vres.code != 0 || rep.Summary.Matched != 2 {
		t.Fatalf("env identity: exit %d: %+v", vres.code, rep)
	}
	rep, vres = verifyRun(t, nil, "--corpus", secret, "--agent", agent)
	if vres.code != 2 || rep.Summary.Invalid != 1 || !strings.Contains(rep.Recordings[0].Message, "bundle is encrypted") {
		t.Fatalf("no key: exit %d: %+v", vres.code, rep)
	}
}
