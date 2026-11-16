package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const verifySchemaVersion = 1

var (
	verifyCorpus  string
	verifyAgent   string
	verifyJSON    bool
	verifyTimeout time.Duration
)

type recordingResult struct {
	Bundle     string `json:"bundle"`
	Verdict    string `json:"verdict"`
	Class      string `json:"class,omitempty"`
	Step       *int   `json:"step,omitempty"`
	Message    string `json:"message,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type verifySummary struct {
	Total    int `json:"total"`
	Matched  int `json:"matched"`
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
}

// verifyExitCode maps a corpus summary to the process exit code. For mixed results the precedence is
// harness error (3) over invalid bundle (2) over divergence (1) over all matched (0).
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
	case verdictDiverged:
		s.Diverged++
	case verdictInvalid:
		s.Invalid++
	default:
		s.Error++
	}
}

func printVerifyReport(rep verifyReport) {
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
		label := map[string]string{verdictMatch: "PASS", verdictDiverged: "DIVERGED", verdictInvalid: "INVALID", verdictError: "ERROR"}[r.Verdict]
		line := fmt.Sprintf("  %-8s  %s", label, r.Bundle)
		switch r.Verdict {
		case verdictDiverged:
			line += fmt.Sprintf("  [%s] step %d: %s", r.Class, *r.Step, r.Message)
		case verdictInvalid, verdictError:
			line += "  " + r.Message
		}
		fmt.Println(line)
	}
	s := rep.Summary
	fmt.Printf("%d passed, %d diverged, %d invalid, %d error  (exit %d)\n", s.Matched, s.Diverged, s.Invalid, s.Error, rep.ExitCode)
}

func runVerify() int {
	if verifyAgent == "" {
		fmt.Fprintln(os.Stderr, "Error: --agent is required")
		return 3
	}
	if verifyCorpus == "" {
		fmt.Fprintln(os.Stderr, "Error: --corpus is required")
		return 3
	}

	names, err := listCorpus(verifyCorpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading corpus: %v\n", err)
		return 3
	}

	rep := verifyReport{SchemaVersion: verifySchemaVersion, Corpus: verifyCorpus, Recordings: []recordingResult{}}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no bundles found")
		rep.ExitCode = 2
		if verifyJSON {
			printVerifyReport(rep)
		}
		return rep.ExitCode
	}

	for _, name := range names {
		start := time.Now()
		o := replayBundle(context.Background(), filepath.Join(verifyCorpus, name), verifyAgent, verifyTimeout)
		rep.Recordings = append(rep.Recordings, toRecordingResult(name, o, time.Since(start)))
		rep.Summary.add(o.Verdict)
	}
	rep.ExitCode = verifyExitCode(rep.Summary)
	printVerifyReport(rep)
	return rep.ExitCode
}

var verifyCmd = &cobra.Command{
	Use:   "verify --corpus <dir> --agent <cmd>",
	Short: "Replay a corpus of recordings against the current agent",
	Long: `Replay every *.zip bundle directly inside --corpus against the agent and report a verdict per recording:
match, diverged (class, step and message), invalid (unreadable, corrupt or undecryptable bundle) or error (harness failure or timeout).

Encrypted bundles are opened with --identity-file / ZYG_IDENTITY_FILE or ZYG_PASSPHRASE_FILE / ZYG_PASSPHRASE; one key applies to the whole corpus.

Exit codes, with precedence for mixed results (highest wins):
  3  harness error: an agent failed or timed out, or the corpus cannot be read
  2  invalid bundle (or an empty corpus)
  1  divergence
  0  every recording matched`,
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
	rootCmd.AddCommand(verifyCmd)
}
