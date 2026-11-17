package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	verifySchemaVersion = 1
	verdictApproved     = "approved"
)

var (
	verifyCorpus    string
	verifyAgent     string
	verifyJSON      bool
	verifyTimeout   time.Duration
	verifyApprovals string
	verifyApprove   []string
	verifyReason    string
	verifyMarkdown  string
)

type recordingResult struct {
	Bundle         string `json:"bundle"`
	Verdict        string `json:"verdict"`
	Class          string `json:"class,omitempty"`
	Step           *int   `json:"step,omitempty"`
	StepName       string `json:"step_name,omitempty"`
	Message        string `json:"message,omitempty"`
	ApprovalReason string `json:"approval_reason,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
}

type verifySummary struct {
	Total    int `json:"total"`
	Matched  int `json:"matched"`
	Approved int `json:"approved"`
	Diverged int `json:"diverged"`
	Invalid  int `json:"invalid"`
	Error    int `json:"error"`
}

type verifyReport struct {
	SchemaVersion int               `json:"schema_version"`
	Corpus        string            `json:"corpus"`
	Summary       verifySummary     `json:"summary"`
	ExitCode      int               `json:"exit_code"`
	Recordings    []recordingResult `json:"recordings"`
	Warnings      []approvalWarning `json:"warnings"`
}

// verifyExitCode maps a corpus summary to the process exit code. For mixed results the precedence is
// harness error (3) over invalid bundle (2) over divergence (1) over all matched (0).
// Approved divergences count as matched.
func verifyExitCode(s verifySummary) int {
	switch {
	case s.Error > 0:
		return 3
	case s.Invalid > 0:
		return 2
	case s.Diverged > 0:
		return 1
	}
	return 0
}

func listCorpus(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			if target, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && target.IsDir() {
				continue
			}
		}
		names = append(names, e.Name())
	}
	return names, nil
}

func toRecordingResult(name string, o replayOutcome, elapsed time.Duration) recordingResult {
	res := recordingResult{Bundle: name, Verdict: o.Verdict, DurationMs: elapsed.Milliseconds()}
	switch o.Verdict {
	case verdictDiverged:
		step := o.Divergence.Step
		res.Class = string(o.Divergence.Class)
		res.Step = &step
		res.StepName = o.StepName
		res.Message = o.Divergence.Message
	case verdictInvalid, verdictError:
		res.Message = o.errMessage()
	}
	return res
}

func (s *verifySummary) add(verdict string) {
	s.Total++
	switch verdict {
	case verdictMatch:
		s.Matched++
	case verdictApproved:
		s.Approved++
	case verdictDiverged:
		s.Diverged++
	case verdictInvalid:
		s.Invalid++
	default:
		s.Error++
	}
}

func printVerifyReport(rep verifyReport, showApproved bool) {
	if verifyJSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error encoding report: %v\n", err)
			return
		}
		fmt.Println(string(b))
		return
	}

	fmt.Printf("zyg verify: %d recordings\n", rep.Summary.Total)
	for _, r := range rep.Recordings {
		label := map[string]string{verdictMatch: "PASS", verdictApproved: "APPROVED", verdictDiverged: "DIVERGED", verdictInvalid: "INVALID", verdictError: "ERROR"}[r.Verdict]
		line := fmt.Sprintf("  %-8s  %s", label, r.Bundle)
		switch r.Verdict {
		case verdictDiverged:
			line += fmt.Sprintf("  [%s] step %d: %s", r.Class, *r.Step, r.Message)
		case verdictApproved:
			line += fmt.Sprintf("  [%s] step %d: %s  (approved: %s)", r.Class, *r.Step, r.Message, r.ApprovalReason)
		case verdictInvalid, verdictError:
			line += "  " + r.Message
		}
		fmt.Println(line)
	}
	s := rep.Summary
	if showApproved {
		fmt.Printf("%d passed, %d approved, %d diverged, %d invalid, %d error  (exit %d)\n", s.Matched, s.Approved, s.Diverged, s.Invalid, s.Error, rep.ExitCode)
		return
	}
	fmt.Printf("%d passed, %d diverged, %d invalid, %d error  (exit %d)\n", s.Matched, s.Diverged, s.Invalid, s.Error, rep.ExitCode)
}

func printStaleWarnings(warnings []approvalWarning) {
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: stale approval %s%s [%s]: %s\n", w.Recording, stepText(w.Step), w.Class, w.why())
	}
}

// approveRecordings records the current divergence of each requested recording in the approvals file.
func approveRecordings(path string, entries []approval, results []recordingResult) bool {
	for _, name := range verifyApprove {
		i := slices.IndexFunc(results, func(r recordingResult) bool { return r.Bundle == name })
		if r := results[i]; r.Verdict != verdictDiverged {
			fmt.Fprintf(os.Stderr, "Error: --approve %s: recording is %s, only a diverged recording can be approved\n", name, r.Verdict)
			return false
		}
	}
	if err := writeApprovals(path, mergeApprovals(entries, results, verifyApprove, verifyReason)); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing approvals file %s: %v\n", path, err)
		return false
	}
	return true
}

// earlyFail writes the markdown report (when requested) for a run that stopped before replaying and returns code.
func earlyFail(code int, msg string, warnings []approvalWarning) int {
	if warnings == nil {
		warnings = []approvalWarning{}
	}
	rep := verifyReport{SchemaVersion: verifySchemaVersion, Corpus: verifyCorpus, ExitCode: code, Recordings: []recordingResult{}, Warnings: warnings}
	writeVerifyMarkdown(rep, msg)
	return code
}

func runVerify() int {
	if verifyAgent == "" {
		fmt.Fprintln(os.Stderr, "Error: --agent is required")
		return earlyFail(3, "--agent is required", nil)
	}
	if verifyCorpus == "" {
		fmt.Fprintln(os.Stderr, "Error: --corpus is required")
		return earlyFail(3, "--corpus is required", nil)
	}
	if len(verifyApprove) > 0 && strings.TrimSpace(verifyReason) == "" {
		fmt.Fprintln(os.Stderr, "Error: --reason is required with --approve")
		return earlyFail(3, "--reason is required with --approve", nil)
	}

	approvalsPath := verifyApprovals
	if approvalsPath == "" {
		approvalsPath = defaultApprovalsPath(verifyCorpus)
	}
	entries, haveApprovals, err := loadApprovals(approvalsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: approvals file %s: %v\n", approvalsPath, err)
		return earlyFail(3, fmt.Sprintf("approvals file %s: %v", approvalsPath, err), nil)
	}

	names, err := listCorpus(verifyCorpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading corpus: %v\n", err)
		return earlyFail(3, fmt.Sprintf("reading corpus: %v", err), nil)
	}
	var unknown []string
	for _, name := range verifyApprove {
		if !slices.Contains(names, name) {
			fmt.Fprintf(os.Stderr, "Error: --approve %s: recording not found in corpus\n", name)
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return earlyFail(3, "--approve: recording not found in corpus: "+strings.Join(unknown, ", "), nil)
	}

	rep := verifyReport{SchemaVersion: verifySchemaVersion, Corpus: verifyCorpus, Recordings: []recordingResult{}, Warnings: []approvalWarning{}}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no bundles found")
		rep.ExitCode = 2
		rep.Warnings = applyApprovals(nil, entries)
		if verifyJSON {
			printVerifyReport(rep, haveApprovals)
		} else {
			printStaleWarnings(rep.Warnings)
		}
		writeVerifyMarkdown(rep, "no bundles found in the corpus")
		return rep.ExitCode
	}

	for _, name := range names {
		start := time.Now()
		o := replayBundle(context.Background(), filepath.Join(verifyCorpus, name), verifyAgent, verifyTimeout)
		rep.Recordings = append(rep.Recordings, toRecordingResult(name, o, time.Since(start)))
	}

	if len(verifyApprove) > 0 {
		if !approveRecordings(approvalsPath, entries, rep.Recordings) {
			return earlyFail(3, "--approve failed; see the job log", nil)
		}
		if entries, _, err = loadApprovals(approvalsPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error: approvals file %s: %v\n", approvalsPath, err)
			return earlyFail(3, fmt.Sprintf("approvals file %s: %v", approvalsPath, err), nil)
		}
		haveApprovals = true
	}

	rep.Warnings = applyApprovals(rep.Recordings, entries)
	for _, r := range rep.Recordings {
		rep.Summary.add(r.Verdict)
	}
	rep.ExitCode = verifyExitCode(rep.Summary)
	printVerifyReport(rep, haveApprovals)
	if !verifyJSON {
		printStaleWarnings(rep.Warnings)
	}
	writeVerifyMarkdown(rep, "")
	return rep.ExitCode
}

var verifyCmd = &cobra.Command{
	Use:   "verify --corpus <dir> --agent <cmd>",
	Short: "Replay a corpus of recordings against the current agent",
	Long: `Replay every *.zip bundle directly inside --corpus against the agent and report a verdict per recording:
match, approved, diverged (class, step and message), invalid (unreadable, corrupt or undecryptable bundle) or error (harness failure or timeout).

Encrypted bundles are opened with --identity-file / ZYG_IDENTITY_FILE or ZYG_PASSPHRASE_FILE / ZYG_PASSPHRASE; one key applies to the whole corpus.

Approvals mark an intentional divergence as expected. They live in <corpus>/.zygote/approvals.yaml (override with --approvals):

  version: 1
  approvals:
    - recording: checkout-flow.zip
      step: 3
      class: effect-mismatch
      reason: "switched to new pricing API"

An approval applies only to a recording diverging at exactly that step with exactly that class; any other divergence is reported as diverged again.
Only diverged recordings can be approved; invalid and error results are never affected. Approved recordings do not fail the run.
A missing approvals file means no approvals. An unreadable or invalid file (bad YAML, unknown field, version other than 1,
empty recording, class or reason, unknown class, duplicate entry) exits 3 before anything is replayed.

Approvals that no longer apply (recording missing from the corpus, now matching, or diverging differently) are reported as stale warnings
(stderr, or "warnings" in --json) and never change the exit code.

To approve current divergences, run verify with --approve <recording> (repeatable) and --reason "<text>". Each named recording must be
diverged; otherwise nothing is written and verify exits 3. The approvals file is rewritten atomically, sorted, keeping other entries.

--markdown <file> additionally writes a Markdown report to <file> (atomically, with or without --json). It starts with a hidden
"<!-- zyg-verify corpus=<corpus> -->" marker line, then the overall result, a table of unapproved divergences (recording, step,
step name, class, message), approved divergences listed separately, invalid and error recordings, and stale approval warnings.
Cells are escaped and messages truncated to 300 characters; sections show at most 50 rows and the whole file stays under 60,000
characters. The file is also written when the run stops early (bad approvals file, unreadable or empty corpus) and contains the error.
A failure to write it is reported on stderr and does not change the exit code.

Exit codes, with precedence for mixed results (highest wins):
  3  harness error: an agent failed or timed out, the corpus cannot be read, or the approvals file or --approve usage is invalid
  2  invalid bundle (or an empty corpus)
  1  unapproved divergence
  0  every recording matched or was approved`,
	Args: cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		os.Exit(runVerify())
	},
}

func init() {
	verifyCmd.Flags().StringVar(&verifyCorpus, "corpus", "", "Directory of *.zip bundles to replay")
	verifyCmd.Flags().StringVar(&verifyAgent, "agent", "", "Agent command to run")
	verifyCmd.Flags().BoolVar(&verifyJSON, "json", false, "Output machine-readable JSON")
	verifyCmd.Flags().DurationVar(&verifyTimeout, "timeout", 5*time.Minute, "Per-recording time limit; a timeout is reported as an error")
	verifyCmd.Flags().StringVar(&verifyApprovals, "approvals", "", "Approvals file (default <corpus>/.zygote/approvals.yaml)")
	verifyCmd.Flags().StringArrayVar(&verifyApprove, "approve", nil, "Approve the current divergence of a recording (repeatable)")
	verifyCmd.Flags().StringVar(&verifyReason, "reason", "", "Reason recorded with --approve")
	verifyCmd.Flags().StringVar(&verifyMarkdown, "markdown", "", "Also write a PR-comment-ready Markdown report to this file")
	rootCmd.AddCommand(verifyCmd)
}
