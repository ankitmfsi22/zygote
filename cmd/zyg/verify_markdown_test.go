package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func markdownRun(t testing.TB, env []string, args ...string) (md string, res zygSplit) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.md")
	res = runZygSplit(t, env, append([]string{"verify", "--markdown", out}, args...)...)
	data, err := os.ReadFile(out) //nolint:gosec
	if err != nil {
		t.Fatalf("markdown report not written (exit %d): %v\nstdout: %s\nstderr: %s", res.code, err, res.stdout, res.stderr)
	}
	return string(data), res
}

func requireContains(t testing.TB, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Fatalf("%s missing %q:\n%s", what, w, text)
		}
	}
}

func requireAbsent(t testing.TB, what, text string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(text, w) {
			t.Fatalf("%s unexpectedly contains %q:\n%s", what, w, text)
		}
	}
}

func TestVerifyStepNameInJSON(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.eff, "c.zip": f.ok})
	writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "new flow")))

	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", f.agent)
	if res.code != 1 {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
	if a := findResult(t, rep, "a.zip"); a.Verdict != "approved" || a.StepName != "other" {
		t.Fatalf("approved row: %+v", a)
	}
	if b := findResult(t, rep, "b.zip"); b.Verdict != "diverged" || b.StepName != "plan" || b.Step == nil || *b.Step != 1 {
		t.Fatalf("effect divergence must carry the recorded step name: %+v", b)
	}
	if c := findResult(t, rep, "c.zip"); c.StepName != "" {
		t.Fatalf("matched row: %+v", c)
	}

	var raw struct {
		Recordings []map[string]any `json:"recordings"`
	}
	if err := json.Unmarshal([]byte(res.stdout), &raw); err != nil {
		t.Fatal(err)
	}
	for _, r := range raw.Recordings {
		_, has := r["step_name"]
		if want := r["bundle"] != "c.zip"; has != want {
			t.Fatalf("%v: step_name present=%v, want %v", r["bundle"], has, want)
		}
	}

	// Effects after the last recorded step exhaust the recording at a step that does not exist, so there is no name.
	extra := fakeAgent(t, "effect|model|mode|"+b64Ok, "step|plan", "effect|model|mode|"+b64Ok)
	rep, _ = verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"e.zip": f.ok}), "--agent", extra)
	if r := rep.Recordings[0]; r.Class != "exhausted" || r.Step == nil || *r.Step != 2 || r.StepName != "" {
		t.Fatalf("exhausted row: %+v", r)
	}

	// A diverged (unapproved) row and an early-stopping agent carry the recorded name of the step that was not reached.
	rep, _ = verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"a.zip": f.div}), "--agent", f.agent)
	if r := rep.Recordings[0]; r.Verdict != "diverged" || r.StepName != "other" {
		t.Fatalf("diverged row: %+v", r)
	}
	rep, _ = verifyRun(t, nil, "--corpus", makeCorpus(t, map[string]string{"a.zip": f.ok}), "--agent", "true")
	if r := rep.Recordings[0]; r.StepName != "plan" {
		t.Fatalf("agent stopped early: %+v", r)
	}

	human := runZygSplit(t, nil, "verify", "--corpus", corpus, "--agent", f.agent)
	requireAbsent(t, "human output", human.stdout, "step_name")
}

func TestVerifyMarkdownReport(t *testing.T) {
	f := newVerifyFixture(t)
	garbage := filepath.Join(t.TempDir(), "garbage.zip")
	writeGarbage(t, garbage)

	t.Run("all pass", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.ok, "b.zip": f.ok})
		md, res := markdownRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 0 {
			t.Fatalf("exit %d", res.code)
		}
		if first := strings.SplitN(md, "\n", 2)[0]; first != "<!-- zyg-verify corpus="+corpus+" -->" {
			t.Fatalf("first line %q", first)
		}
		requireContains(t, "markdown", md, "## zyg verify: all passed (exit 0)", "2 recordings: 2 passed, 0 approved, 0 diverged, 0 invalid, 0 error")
		requireAbsent(t, "markdown", md, "### ")
	})

	t.Run("diverged", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.ok})
		md, res := markdownRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 1 {
			t.Fatalf("exit %d", res.code)
		}
		requireContains(t, "markdown", md,
			"## zyg verify: 1 of 2 diverged (exit 1)",
			"### Diverged (1)",
			"| Recording | Step | Step name | Class | Message |",
			`| a.zip | 1 | other | step-mismatch | expected "other", got "plan" |`)
		requireAbsent(t, "markdown", md, "b.zip", "Approved divergences")
	})

	t.Run("approved only", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.div})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "new flow")))
		md, res := markdownRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 0 {
			t.Fatalf("exit %d", res.code)
		}
		requireContains(t, "markdown", md,
			"## zyg verify: all passed (exit 0)",
			"### Approved divergences (1)",
			"do not fail the run",
			"| Recording | Step | Step name | Class | Reason |",
			"| a.zip | 1 | other | step-mismatch | new flow |")
		requireAbsent(t, "markdown", md, "### Diverged")
	})

	t.Run("mixed", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"ok.zip": f.ok, "div.zip": f.div, "bad.zip": garbage, "err.zip": f.errB, "appr.zip": f.eff})
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(
			entryYAML("appr.zip", 1, "effect-mismatch", "swapped model"),
			entryYAML("gone.zip", 2, "step-mismatch", "old")))
		md, res := markdownRun(t, []string{"ZYG_TEST_FAIL=1"}, "--corpus", corpus, "--agent", f.agent, "--json")
		if res.code != 3 {
			t.Fatalf("exit %d: %s", res.code, res.stdout)
		}
		requireContains(t, "markdown", md,
			"## zyg verify: error (exit 3)",
			"5 recordings: 1 passed, 1 approved, 1 diverged, 1 invalid, 1 error",
			"### Diverged (1)", "| div.zip | 1 | other |",
			"### Invalid and error recordings (2)", "| Recording | Result | Message |", "| bad.zip | invalid |", "| err.zip | error | agent exited with error: exit status 4 |",
			"### Approved divergences (1)", "| appr.zip | 1 | plan | effect-mismatch | swapped model |",
			"### Stale approvals (1)", "- gone.zip step 2 [step-mismatch]: recording is not in the corpus")
		requireAbsent(t, "markdown", md, "ok.zip")
		var rep verifyReport
		if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil || rep.ExitCode != 3 {
			t.Fatalf("--json output must be unaffected: %v\n%s", err, res.stdout)
		}
	})
}

func TestVerifyMarkdownEscapingAndLimits(t *testing.T) {
	step := 3
	nasty := "a|b\nline2 `code` <script>alert(1)</script> & @team"
	rep := verifyReport{Corpus: "dir--with -->odd\nname", ExitCode: 1, Recordings: []recordingResult{
		{Bundle: "x|y.zip", Verdict: verdictDiverged, Class: "effect-mismatch", Step: &step, StepName: "s|<b>", Message: nasty},
		{Bundle: "ok.zip", Verdict: verdictApproved, Class: "step-mismatch", Step: &step, ApprovalReason: nasty},
	}, Summary: verifySummary{Total: 2, Diverged: 1, Approved: 1}}

	t.Run("escaping", func(t *testing.T) {
		md := renderVerifyMarkdown(rep, "")
		first := strings.SplitN(md, "\n", 2)[0]
		if strings.Count(first, "--") != 2 || !strings.HasPrefix(first, "<!-- zyg-verify corpus=") || !strings.HasSuffix(first, " -->") {
			t.Fatalf("marker must stay a single well-formed comment line: %q", first)
		}
		requireAbsent(t, "markdown", md, "<script>", "<b>", "a|b", "@team")
		requireContains(t, "markdown", md, `a\|b line2 \`+"`code\\`"+` &lt;script&gt;alert(1)&lt;/script&gt; &amp; @`+"\u200b"+"team", `x\|y.zip`, `s\|&lt;b&gt;`)
		for _, line := range strings.Split(md, "\n") {
			if strings.HasPrefix(line, "| ") && strings.Count(strings.ReplaceAll(line, `\|`, ""), "|") != 6 {
				t.Fatalf("row broke the table: %q", line)
			}
		}
	})

	t.Run("truncates messages to 300 characters", func(t *testing.T) {
		long := strings.Repeat("é", 1000)
		r := rep
		r.Recordings = []recordingResult{{Bundle: "a.zip", Verdict: verdictDiverged, Class: "step-mismatch", Step: &step, Message: long}}
		md := renderVerifyMarkdown(r, "")
		if strings.Contains(md, strings.Repeat("é", 301)) || !strings.Contains(md, strings.Repeat("é", 300)+"…") || !utf8.ValidString(md) {
			t.Fatalf("message not truncated to 300 characters")
		}
	})

	t.Run("limits rows to 50", func(t *testing.T) {
		r := verifyReport{Corpus: "c", ExitCode: 1}
		for i := range 120 {
			r.Recordings = append(r.Recordings, recordingResult{Bundle: fmt.Sprintf("r%03d.zip", i), Verdict: verdictDiverged, Class: "step-mismatch", Step: &step, Message: "m"})
		}
		md := renderVerifyMarkdown(r, "")
		if n := strings.Count(md, "step-mismatch |"); n != 50 {
			t.Fatalf("%d rows shown, want 50", n)
		}
		requireContains(t, "markdown", md, "r049.zip", "...and 70 more", "### Diverged (120)")
		requireAbsent(t, "markdown", md, "r050.zip")
	})

	t.Run("caps total size", func(t *testing.T) {
		huge := strings.Repeat("|", 400)
		r := verifyReport{Corpus: "c", ExitCode: 3}
		for i := range 200 {
			name := fmt.Sprintf("%s%03d", huge, i)
			rr := recordingResult{Bundle: name, Verdict: verdictDiverged, Class: "step-mismatch", Step: &step, StepName: huge, Message: huge}
			r.Recordings = append(r.Recordings, rr)
			rr.Verdict, rr.ApprovalReason = verdictApproved, huge
			r.Recordings = append(r.Recordings, rr)
			rr.Verdict, rr.Message = verdictInvalid, huge
			r.Recordings = append(r.Recordings, rr)
			r.Warnings = append(r.Warnings, approvalWarning{Recording: name, Class: "step-mismatch", Problem: problemMissing})
		}
		md := renderVerifyMarkdown(r, huge)
		if len(md) > 60000 {
			t.Fatalf("report is %d bytes, want <= 60000", len(md))
		}
		requireContains(t, "markdown", md, "<!-- zyg-verify corpus=c -->", "Report truncated")
		if !utf8.ValidString(md) {
			t.Fatal("truncation split a character")
		}
	})
}

func TestVerifyMarkdownOnEarlyFailure(t *testing.T) {
	f := newVerifyFixture(t)

	t.Run("invalid approvals file", func(t *testing.T) {
		corpus := makeCorpus(t, map[string]string{"a.zip": f.ok})
		writeApprovalsFile(t, approvalsPathIn(corpus), "version: 7\napprovals: []\n")
		dir := t.TempDir()
		out := filepath.Join(dir, "sub", "report.md")
		res := runZygSplit(t, nil, "verify", "--markdown", out, "--corpus", corpus, "--agent", f.agent)
		if res.code != 3 {
			t.Fatalf("exit %d", res.code)
		}
		md := readFileString(t, out)
		requireContains(t, "markdown", md, "<!-- zyg-verify corpus="+corpus+" -->", "## zyg verify: error (exit 3)", "**Error:** approvals file", "unsupported version 7")
		entries, err := os.ReadDir(filepath.Dir(out))
		if err != nil || len(entries) != 1 {
			t.Fatalf("atomic write left stray files: %v %v", entries, err)
		}
	})

	t.Run("empty corpus", func(t *testing.T) {
		corpus := t.TempDir()
		writeApprovalsFile(t, approvalsPathIn(corpus), approvalYAML(entryYAML("a.zip", 1, "step-mismatch", "old")))
		md, res := markdownRun(t, nil, "--corpus", corpus, "--agent", f.agent)
		if res.code != 2 {
			t.Fatalf("exit %d", res.code)
		}
		requireContains(t, "markdown", md, "## zyg verify: invalid (exit 2)", "**Error:** no bundles found", "### Stale approvals (1)")
	})

	t.Run("unreadable corpus", func(t *testing.T) {
		md, res := markdownRun(t, nil, "--corpus", filepath.Join(t.TempDir(), "nope"), "--agent", f.agent)
		if res.code != 3 {
			t.Fatalf("exit %d", res.code)
		}
		requireContains(t, "markdown", md, "## zyg verify: error (exit 3)", "**Error:** reading corpus")
	})
}

var durationRe = regexp.MustCompile(`"duration_ms": \d+`)

func TestVerifyNoMarkdownFlagUnchanged(t *testing.T) {
	f := newVerifyFixture(t)
	corpus := makeCorpus(t, map[string]string{"a.zip": f.div, "b.zip": f.ok})
	base := []string{"--corpus", corpus, "--agent", f.agent}

	plain := runZygSplit(t, nil, append([]string{"verify"}, base...)...)
	withMD := runZygSplit(t, nil, append([]string{"verify", "--markdown", filepath.Join(t.TempDir(), "r.md")}, base...)...)
	if plain != withMD {
		t.Fatalf("--markdown changed human output:\n%+v\nvs\n%+v", plain, withMD)
	}

	jsonPlain := runZygSplit(t, nil, append([]string{"verify", "--json"}, base...)...)
	jsonMD := runZygSplit(t, nil, append([]string{"verify", "--json", "--markdown", filepath.Join(t.TempDir(), "r.md")}, base...)...)
	norm := func(s string) string { return durationRe.ReplaceAllString(s, `"duration_ms": 0`) }
	if norm(jsonPlain.stdout) != norm(jsonMD.stdout) || jsonPlain.code != jsonMD.code {
		t.Fatalf("--markdown changed JSON output:\n%s\nvs\n%s", jsonPlain.stdout, jsonMD.stdout)
	}

	if out := runZygSplit(t, nil, "verify", "--help"); !strings.Contains(out.stdout, "--markdown string") {
		t.Fatalf("--markdown missing from help:\n%s", out.stdout)
	}
}

// promptAgent returns a shell agent that reads a per-run input through an effect, assembles a prompt from it and
// sends it as an llm-like effect keyed by the prompt, then closes a step. Version 2 changes only the prompt-assembly
// logic, and only for inputs that mention a refund; the step names are identical in both versions.
func promptAgent(t testing.TB, version int) string {
	t.Helper()
	assemble := `prompt="Summarize the ticket: $input"`
	if version == 2 {
		assemble += "\ncase \"$input\" in *refund*) prompt=\"[PRIORITY:billing] Summarize the ticket: $input\";; esac"
	}
	script := `rpc() { printf '%s\n' "$1"; IFS= read -r _line; }
rpc '{"jsonrpc":"2.0","id":1,"method":"handshake","params":{"version":1}}'
in_b64=$(printf %s "$ZYG_TEST_INPUT" | base64 | tr -d '\n')
rpc "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"effect\",\"params\":{\"op\":\"tool:read\",\"key\":\"ticket\",\"value\":\"$in_b64\"}}"
got=$(printf '%s' "$_line" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
input=$(printf %s "$got" | base64 -d)
rpc '{"jsonrpc":"2.0","id":3,"method":"step","params":{"name":"read-ticket"}}'
` + assemble + `
p_b64=$(printf %s "$prompt" | base64 | tr -d '\n')
sum=$(printf %s "$prompt" | cksum | cut -d' ' -f1)
rpc "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"effect\",\"params\":{\"op\":\"model\",\"key\":\"complete-$sum\",\"value\":\"$p_b64\"}}"
rpc '{"jsonrpc":"2.0","id":5,"method":"step","params":{"name":"answer"}}'
`
	path := filepath.Join(t.TempDir(), "prompt-agent.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return "sh '" + filepath.ToSlash(path) + "'"
}

func TestVerifyExitCriterion3of40(t *testing.T) {
	requireShell(t)
	const total = 40
	refunds := map[int]bool{7: true, 19: true, 33: true}
	v1 := promptAgent(t, 1)

	files := map[string]string{}
	for i := range total {
		input := fmt.Sprintf("ticket %d: the login page shows an error", i)
		if refunds[i] {
			input = fmt.Sprintf("ticket %d: the customer asks for a refund", i)
		}
		files[fmt.Sprintf("ticket-%02d.zip", i)] = recordFake(t, []string{"ZYG_TEST_INPUT=" + input}, v1)
	}
	corpus := makeCorpus(t, files)

	if rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", v1); res.code != 0 || rep.Summary.Matched != total {
		t.Fatalf("v1 must match its own recordings: exit %d: %+v\n%s", res.code, rep.Summary, res.stderr)
	}

	start := time.Now()
	out := filepath.Join(t.TempDir(), "report.md")
	rep, res := verifyRun(t, nil, "--corpus", corpus, "--agent", promptAgent(t, 2), "--markdown", out)
	elapsed := time.Since(start)

	if res.code != 1 || rep.Summary.Diverged != 3 || rep.Summary.Matched != total-3 || rep.Summary.Total != total {
		t.Fatalf("exit %d: %+v", res.code, rep.Summary)
	}
	if elapsed >= 2*time.Minute {
		t.Fatalf("verifying %d recordings took %s, want under 2m (SRS 11.5)", total, elapsed)
	}

	md := readFileString(t, out)
	requireContains(t, "markdown", md, "## zyg verify: 3 of 40 diverged (exit 1)", "### Diverged (3)")
	rows := map[string]string{}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| ticket-") {
			rows[strings.Fields(line)[1]] = line
		}
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows name a ticket, want 3:\n%s", len(rows), md)
	}
	for i := range refunds {
		name := fmt.Sprintf("ticket-%02d.zip", i)
		r := findResult(t, rep, name)
		if r.Verdict != "diverged" || r.Class != "effect-mismatch" || r.Step == nil || *r.Step != 2 || r.StepName != "answer" {
			t.Fatalf("%s: %+v", name, r)
		}
		want := "| " + name + " | 2 | answer | effect-mismatch |"
		if !strings.HasPrefix(rows[name], want) {
			t.Fatalf("row for %s = %q, want prefix %q", name, rows[name], want)
		}
	}
	t.Logf("verified %d recordings in %s", total, elapsed)
}
