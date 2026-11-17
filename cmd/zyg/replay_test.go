package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func replayFixture(t *testing.T) string {
	t.Helper()
	requireShell(t)
	return recordFake(t, nil, fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan", "step|act"))
}

func TestReplayOutputUnchanged(t *testing.T) {
	bundle := replayFixture(t)
	good := fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan", "step|act")
	badStep := fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan", "step|other")
	badEffect := fakeAgent(t, "effect|model|z|"+b64Ok, "step|plan", "step|act")
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)

	const stepMsg = `expected "act", got "other"`
	tests := []struct {
		name       string
		args       []string
		wantStdout string
		wantStderr string
		wantCode   int
	}{
		{"match", []string{"replay", bundle, "--agent", good}, "Replay matched all steps perfectly.\n", "", 0},
		{"match json", []string{"replay", bundle, "--agent", good, "--json"}, "{\"status\": \"match\"}\n", "", 0},
		{"step mismatch", []string{"replay", bundle, "--agent", badStep}, "", "[!] DIVERGENCE DETECTED [step-mismatch] at step 2: " + stepMsg + "\n", 1},
		{"step mismatch json", []string{"replay", bundle, "--agent", badStep, "--json"},
			`{"status":"diverged","class":"step-mismatch","step":2,"message":"expected \"act\", got \"other\""}` + "\n", "", 1},
		{"effect mismatch", []string{"replay", bundle, "--agent", badEffect}, "", "[!] DIVERGENCE DETECTED [effect-mismatch] at step 1: different op/key requested than recorded\n", 1},
		{"invalid bundle", []string{"replay", garbage, "--agent", good}, "", "Error importing bundle: zip: not a valid zip file\n", 2},
		{"invalid bundle json", []string{"replay", garbage, "--agent", good, "--json"}, `{"status":"error","message":"zip: not a valid zip file"}` + "\n", "", 2},
		{"missing agent", []string{"replay", bundle}, "", "Error: --agent is required\n", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runZygSplit(t, nil, tt.args...)
			if res.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.wantCode, res.stdout, res.stderr)
			}
			if res.stdout != tt.wantStdout {
				t.Fatalf("stdout %q, want %q", res.stdout, tt.wantStdout)
			}
			if res.stderr != tt.wantStderr {
				t.Fatalf("stderr %q, want %q", res.stderr, tt.wantStderr)
			}
		})
	}
}

func TestReplayAgentStoppedEarly(t *testing.T) {
	bundle := replayFixture(t)

	res := runZygSplit(t, nil, "replay", bundle, "--agent", "true")
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	want := "[!] DIVERGENCE DETECTED [step-mismatch] at step 1: agent stopped after step 0 of 2\n"
	if res.stderr != want {
		t.Fatalf("stderr %q, want %q", res.stderr, want)
	}

	partial := fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan")
	res = runZygSplit(t, nil, "replay", bundle, "--agent", partial, "--json")
	if res.code != 1 || !strings.Contains(res.stdout, `"class":"step-mismatch"`) || !strings.Contains(res.stdout, "agent stopped after step 1 of 2") {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
}

func TestReplayAgentSkippedTrailingEffect(t *testing.T) {
	requireShell(t)
	bundle := recordFake(t, nil, fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan", "effect|model|late|"+b64Ok))

	full := fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan", "effect|model|late|"+b64Ok)
	if res := runZygSplit(t, nil, "replay", bundle, "--agent", full); res.code != 0 {
		t.Fatalf("full agent exit %d: %s%s", res.code, res.stdout, res.stderr)
	}

	short := fakeAgent(t, "effect|model|k|"+b64Ok, "step|plan")
	res := runZygSplit(t, nil, "replay", bundle, "--agent", short)
	if res.code != 1 || !strings.Contains(res.stderr, "[step-mismatch]") || !strings.Contains(res.stderr, "agent stopped after consuming 1 of 2 recorded effects") {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
}
