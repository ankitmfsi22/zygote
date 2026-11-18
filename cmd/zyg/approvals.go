package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mindfire/zygote/journal"
	"gopkg.in/yaml.v3"
)

const approvalsVersion = 1

const (
	problemMissing         = "missing"
	problemMatches         = "matches"
	problemNoLongerApplies = "no-longer-applies"
)

var knownClasses = []string{
	string(journal.ClassEffectMismatch),
	string(journal.ClassWorldMismatch),
	string(journal.ClassStepMismatch),
	string(journal.ClassExhausted),
	string(journal.ClassMissingObject),
	string(journal.ClassUninstrumented),
}

type approval struct {
	Recording string `yaml:"recording"`
	Step      *int   `yaml:"step,omitempty"`
	Class     string `yaml:"class"`
	Reason    string `yaml:"reason"`
}

type approvalsFile struct {
	Version   int        `yaml:"version"`
	Approvals []approval `yaml:"approvals"`
}

type approvalWarning struct {
	Recording string `json:"recording"`
	Step      *int   `json:"step,omitempty"`
	Class     string `json:"class"`
	Reason    string `json:"reason"`
	Problem   string `json:"problem"`
}

func defaultApprovalsPath(corpus string) string {
	return filepath.Join(corpus, ".zygote", "approvals.yaml")
}

func corpusApprovalsPath(corpus string, isFile bool) string {
	if isFile {
		return defaultApprovalsPath(filepath.Dir(corpus))
	}
	return defaultApprovalsPath(corpus)
}

func sameStep(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func stepText(step *int) string {
	if step == nil {
		return ""
	}
	return fmt.Sprintf(" step %d", *step)
}

func compareApprovals(a, b approval) int {
	if c := strings.Compare(a.Recording, b.Recording); c != 0 {
		return c
	}
	switch {
	case a.Step == nil && b.Step != nil:
		return -1
	case a.Step != nil && b.Step == nil:
		return 1
	case a.Step != nil && *a.Step != *b.Step:
		return *a.Step - *b.Step
	}
	return strings.Compare(a.Class, b.Class)
}

func (a approval) sameKey(b approval) bool {
	return a.Recording == b.Recording && sameStep(a.Step, b.Step) && a.Class == b.Class
}

// loadApprovals reads the approvals file; a missing file is not an error and reports exists=false.
func loadApprovals(path string) (entries []approval, exists bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}

	var f approvalsFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, true, errors.New("file is empty")
		}
		return nil, true, err
	}
	if err := validateApprovals(f); err != nil {
		return nil, true, err
	}
	return f.Approvals, true, nil
}

func validateApprovals(f approvalsFile) error {
	if f.Version != approvalsVersion {
		return fmt.Errorf("unsupported version %d, want %d", f.Version, approvalsVersion)
	}
	for i, a := range f.Approvals {
		where := fmt.Sprintf("approvals[%d]", i)
		switch {
		case strings.TrimSpace(a.Recording) == "":
			return fmt.Errorf("%s: recording is required", where)
		case strings.TrimSpace(a.Class) == "":
			return fmt.Errorf("%s: class is required", where)
		case !slices.Contains(knownClasses, a.Class):
			return fmt.Errorf("%s: unknown class %q", where, a.Class)
		case strings.TrimSpace(a.Reason) == "":
			return fmt.Errorf("%s: reason is required", where)
		case a.Step != nil && *a.Step < 0:
			return fmt.Errorf("%s: step must not be negative", where)
		}
		for _, prev := range f.Approvals[:i] {
			if prev.sameKey(a) {
				return fmt.Errorf("%s: duplicate approval for %s%s [%s]", where, a.Recording, stepText(a.Step), a.Class)
			}
		}
	}
	return nil
}

// applyApprovals marks each approved divergence in place and returns a warning for every approval that did not apply.
func applyApprovals(results []recordingResult, entries []approval) []approvalWarning {
	warnings := []approvalWarning{}
	used := make([]bool, len(entries))
	for i := range results {
		r := &results[i]
		if r.Verdict != verdictDiverged {
			continue
		}
		for j, a := range entries {
			if a.Recording == r.Bundle && sameStep(a.Step, r.Step) && a.Class == r.Class {
				r.Verdict = verdictApproved
				r.ApprovalReason = a.Reason
				used[j] = true
				break
			}
		}
	}
	for j, a := range entries {
		if used[j] {
			continue
		}
		problem := problemMissing
		for _, r := range results {
			if r.Bundle != a.Recording {
				continue
			}
			problem = problemNoLongerApplies
			if r.Verdict == verdictMatch {
				problem = problemMatches
			}
			break
		}
		warnings = append(warnings, approvalWarning{Recording: a.Recording, Step: a.Step, Class: a.Class, Reason: a.Reason, Problem: problem})
	}
	return warnings
}

func (w approvalWarning) why() string {
	switch w.Problem {
	case problemMissing:
		return "recording is not in the corpus"
	case problemMatches:
		return "recording now matches"
	}
	return "recording no longer diverges this way"
}

// mergeApprovals adds or updates the approvals for the given diverged results and returns the sorted list.
func mergeApprovals(entries []approval, results []recordingResult, names []string, reason string) []approval {
	merged := slices.Clone(entries)
	for _, name := range names {
		for _, r := range results {
			if r.Bundle != name {
				continue
			}
			next := approval{Recording: name, Step: r.Step, Class: r.Class, Reason: reason}
			if i := slices.IndexFunc(merged, next.sameKey); i >= 0 {
				merged[i] = next
			} else {
				merged = append(merged, next)
			}
		}
	}
	slices.SortStableFunc(merged, compareApprovals)
	return merged
}

func writeApprovals(path string, entries []approval) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(approvalsFile{Version: approvalsVersion, Approvals: entries}); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".approvals-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(buf.Bytes())
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
