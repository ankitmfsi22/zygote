package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	markdownMaxCell  = 300
	markdownMaxRows  = 50
	markdownMaxBytes = 60000
	markdownTruncate = "\n\n_Report truncated to fit the size limit; see the JSON report for the full result._\n"
)

// markdownMarker is the hidden first line that lets a CI job find and update its own PR comment per corpus.
func markdownMarker(corpus string) string {
	c := strings.Join(strings.Fields(corpus), " ")
	for strings.Contains(c, "--") {
		c = strings.ReplaceAll(c, "--", "-")
	}
	return "<!-- zyg-verify corpus=" + c + " -->"
}

// mdText makes untrusted text safe for a table cell or paragraph: it truncates to max characters, flattens control
// characters and newlines, and escapes pipes, backticks, HTML and @mentions.
func mdText(s string, limit int) string {
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit]) + "…"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '|':
			b.WriteString(`\|`)
		case r == '`':
			b.WriteString("\\`")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '&':
			b.WriteString("&amp;")
		case r == '@':
			b.WriteString("@")
			b.WriteRune(0x200b) // zero-width space stops the mention from pinging
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mdCell(s string) string {
	if s == "" {
		return "-"
	}
	return mdText(s, markdownMaxCell)
}

func mdStep(step *int) string {
	if step == nil {
		return "-"
	}
	return fmt.Sprint(*step)
}

func mdHeading(rep verifyReport) string {
	s := rep.Summary
	var status string
	switch rep.ExitCode {
	case 0:
		status = "all passed"
	case 1:
		status = fmt.Sprintf("%d of %d diverged", s.Diverged, s.Total)
	case 2:
		status = "invalid"
	default:
		status = "error"
	}
	return fmt.Sprintf("## zyg verify: %s (exit %d)\n", status, rep.ExitCode)
}

// mdTable renders rows under header, showing at most markdownMaxRows of them.
func mdTable(b *strings.Builder, header []string, rows [][]string) {
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(header)) + "\n")
	for i, row := range rows {
		if i == markdownMaxRows {
			break
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	if len(rows) > markdownMaxRows {
		fmt.Fprintf(b, "\n...and %d more\n", len(rows)-markdownMaxRows)
	}
}

// renderVerifyMarkdown builds the PR-comment report. errText, when set, is a failure that stopped the run early.
func renderVerifyMarkdown(rep verifyReport, errText string) string {
	var b strings.Builder
	b.WriteString(markdownMarker(rep.Corpus) + "\n")
	b.WriteString(mdHeading(rep))

	s := rep.Summary
	fmt.Fprintf(&b, "\nCorpus: %s\n", mdText(rep.Corpus, markdownMaxCell))
	if errText != "" {
		fmt.Fprintf(&b, "\n**Error:** %s\n", mdText(errText, markdownMaxCell))
	} else {
		fmt.Fprintf(&b, "\n%d recordings: %d passed, %d approved, %d diverged, %d invalid, %d error\n", s.Total, s.Matched, s.Approved, s.Diverged, s.Invalid, s.Error)
	}

	var diverged, approved, failed [][]string
	for _, r := range rep.Recordings {
		switch r.Verdict {
		case verdictDiverged:
			diverged = append(diverged, []string{mdCell(r.Bundle), mdStep(r.Step), mdCell(r.StepName), mdCell(r.Class), mdCell(r.Message)})
		case verdictApproved:
			approved = append(approved, []string{mdCell(r.Bundle), mdStep(r.Step), mdCell(r.StepName), mdCell(r.Class), mdCell(r.ApprovalReason)})
		case verdictInvalid, verdictError:
			failed = append(failed, []string{mdCell(r.Bundle), mdCell(r.Verdict), mdCell(r.Message)})
		}
	}

	if len(diverged) > 0 {
		fmt.Fprintf(&b, "\n### Diverged (%d)\n\n", len(diverged))
		mdTable(&b, []string{"Recording", "Step", "Step name", "Class", "Message"}, diverged)
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n### Invalid and error recordings (%d)\n\n", len(failed))
		mdTable(&b, []string{"Recording", "Result", "Message"}, failed)
	}
	if len(approved) > 0 {
		fmt.Fprintf(&b, "\n### Approved divergences (%d)\n\nThese divergences are approved and do not fail the run.\n\n", len(approved))
		mdTable(&b, []string{"Recording", "Step", "Step name", "Class", "Reason"}, approved)
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprintf(&b, "\n### Stale approvals (%d)\n\n", len(rep.Warnings))
		for i, w := range rep.Warnings {
			if i == markdownMaxRows {
				fmt.Fprintf(&b, "\n...and %d more\n", len(rep.Warnings)-markdownMaxRows)
				break
			}
			fmt.Fprintf(&b, "- %s%s [%s]: %s\n", mdText(w.Recording, markdownMaxCell), stepText(w.Step), mdText(w.Class, markdownMaxCell), w.why())
		}
	}

	out := b.String()
	if len(out) > markdownMaxBytes {
		cut := markdownMaxBytes - len(markdownTruncate)
		if i := strings.LastIndexByte(out[:cut], '\n'); i > 0 {
			cut = i
		}
		out = out[:cut] + markdownTruncate
	}
	return out
}

// writeFileAtomic writes data to path via a temporary file in the same directory followed by a rename.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".zyg-markdown-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// writeVerifyMarkdown writes the --markdown report if requested. A write failure is reported but never changes the exit code.
func writeVerifyMarkdown(rep verifyReport, errText string) {
	if verifyMarkdown == "" {
		return
	}
	if err := writeFileAtomic(verifyMarkdown, []byte(renderVerifyMarkdown(rep, errText))); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing markdown report %s: %v\n", verifyMarkdown, err)
	}
}
