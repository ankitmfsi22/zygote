package trace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/mindfire/zygote/vfs"
)

// MergeStats describes a merged archive.
type MergeStats struct {
	// Runs is the number of runs in the archive.
	Runs int
	// Objects is the number of distinct objects stored.
	Objects int
	// Shared is the number of objects used by more than one run.
	Shared int
}

// SplitParams selects what SplitFile writes.
type SplitParams struct {
	// Runs limits the split to the named runs; empty means every run.
	Runs []string
	// Force allows overwriting existing output files.
	Force bool
}

// writeTemp writes through write (encrypting to recipients when there are any) into a temporary file in dir
// and returns its path. The caller renames it into place; on error nothing is left behind.
func writeTemp(dir string, recipients []age.Recipient, write func(io.Writer) error) (path string, err error) {
	tmp, err := os.CreateTemp(dir, ".zyg-*.tmp")
	if err != nil {
		return "", err
	}
	path = tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(path)
		}
	}()

	if len(recipients) > 0 {
		w, encErr := age.Encrypt(tmp, recipients...)
		if encErr != nil {
			return "", encErr
		}
		if err = write(w); err != nil {
			return "", err
		}
		if err = w.Close(); err != nil {
			return "", err
		}
	} else if err = write(tmp); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// runNameFromPath derives a run name from an input file name, dropping a .zip extension.
func runNameFromPath(path string) (string, error) {
	base := filepath.Base(path)
	if strings.EqualFold(filepath.Ext(base), ".zip") {
		base = base[:len(base)-len(".zip")]
	}
	if err := validRunName(base); err != nil {
		return "", &InputError{Path: path, Err: err}
	}
	return base, nil
}

// MergeFiles combines single-run bundles and multi-run archives into one multi-run archive at out.
// Single-run inputs are named after their file name without .zip. Every input is validated before anything is
// written, and the output is written atomically. Output is encrypted only when exportOpts ask for it.
func MergeFiles(out string, inputs []string, importOpts []ImportOption, exportOpts []ExportOption) (MergeStats, error) {
	var cfg exportConfig
	for _, opt := range exportOpts {
		opt(&cfg)
	}
	recipients, err := cfg.ageRecipients()
	if err != nil {
		return MergeStats{}, err
	}

	var runs []archiveRun
	source := make(map[string]string)
	objects := make(map[vfs.Hash][]byte)
	uses := make(map[vfs.Hash]int)
	for _, in := range inputs {
		a, err := OpenArchive(in, importOpts...)
		if err != nil {
			return MergeStats{}, &InputError{Path: in, Err: err}
		}
		for _, r := range a.runs {
			if !a.multi {
				if r.name, err = runNameFromPath(in); err != nil {
					return MergeStats{}, err
				}
			}
			if prev, dup := source[r.name]; dup {
				return MergeStats{}, fmt.Errorf("duplicate run name %q: %s and %s", r.name, prev, in)
			}
			source[r.name] = in

			hashes, err := r.needed(a.store)
			if err != nil {
				return MergeStats{}, &InputError{Path: in, Err: ErrMissingObject}
			}
			for _, h := range hashes {
				if _, ok := objects[h]; !ok {
					data, ok := a.store.Get(h)
					if !ok {
						return MergeStats{}, &InputError{Path: in, Err: ErrMissingObject}
					}
					objects[h] = data
				}
				uses[h]++
			}
			runs = append(runs, r)
		}
	}
	if len(runs) < 2 {
		return MergeStats{}, fmt.Errorf("merge needs at least 2 runs, got %d", len(runs))
	}
	slices.SortFunc(runs, func(a, b archiveRun) int { return strings.Compare(a.name, b.name) })

	stats := MergeStats{Runs: len(runs), Objects: len(objects)}
	blobs := make([]blob, 0, len(objects))
	for h, data := range objects {
		blobs = append(blobs, blob{h, data})
		if uses[h] > 1 {
			stats.Shared++
		}
	}
	slices.SortFunc(blobs, func(a, b blob) int { return strings.Compare(string(a.hash[:]), string(b.hash[:])) })

	tmp, err := writeTemp(filepath.Dir(out), recipients, func(w io.Writer) error {
		return writeMultiRunZip(w, runs, blobs)
	})
	if err != nil {
		return MergeStats{}, err
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return MergeStats{}, err
	}
	return stats, nil
}

// SplitFile writes each selected run of a multi-run archive to dir/<run>.zip as a standard single-run bundle
// whose trace.json is the run's original bytes, and returns the files written in run order. The archive is
// validated first and every output is staged before any is moved into place. Existing files are refused
// unless p.Force is set. Output is encrypted only when exportOpts ask for it.
func SplitFile(archive, dir string, p SplitParams, importOpts []ImportOption, exportOpts []ExportOption) ([]string, error) {
	var cfg exportConfig
	for _, opt := range exportOpts {
		opt(&cfg)
	}
	recipients, err := cfg.ageRecipients()
	if err != nil {
		return nil, err
	}

	a, err := OpenArchive(archive, importOpts...)
	if err != nil {
		return nil, &InputError{Path: archive, Err: err}
	}
	if !a.multi {
		return nil, &InputError{Path: archive, Err: ErrSingleRunBundle}
	}

	selected := a.runs
	if len(p.Runs) > 0 {
		selected = nil
		for _, name := range slices.Compact(slices.Sorted(slices.Values(p.Runs))) {
			r, err := a.run(name)
			if err != nil {
				return nil, err
			}
			selected = append(selected, r)
		}
	}

	archiveInfo, err := os.Stat(archive)
	if err != nil {
		return nil, err
	}
	targets := make([]string, len(selected))
	lower := make(map[string]string, len(selected))
	for i, r := range selected {
		targets[i] = filepath.Join(dir, r.name+".zip")
		key := strings.ToLower(r.name)
		if prev, dup := lower[key]; dup {
			return nil, fmt.Errorf("runs %q and %q would write the same file on a case-insensitive filesystem; split them with --run", prev, r.name)
		}
		lower[key] = r.name

		info, err := os.Lstat(targets[i])
		switch {
		case err == nil && os.SameFile(info, archiveInfo):
			return nil, fmt.Errorf("refusing to overwrite the input archive %s", targets[i])
		case err == nil && (info.IsDir() || !p.Force):
			return nil, fmt.Errorf("refusing to overwrite %s (use --force)", targets[i])
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	temps := make([]string, 0, len(selected))
	cleanup := func() {
		for _, t := range temps {
			_ = os.Remove(t)
		}
	}
	for _, r := range selected {
		blobs, err := a.blobsFor(r)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("run %q: %w", r.name, err)
		}
		tmp, err := writeTemp(dir, recipients, func(w io.Writer) error {
			return writeSingleRunZip(w, r.manifest, blobs)
		})
		if err != nil {
			cleanup()
			return nil, err
		}
		temps = append(temps, tmp)
	}
	for i, tmp := range temps {
		if err := os.Rename(tmp, targets[i]); err != nil {
			cleanup()
			return nil, err
		}
	}
	return targets, nil
}
