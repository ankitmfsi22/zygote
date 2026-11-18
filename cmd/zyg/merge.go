package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/mindfire/zygote/trace"
	"github.com/spf13/cobra"
)

var (
	mergeOut   string
	mergeForce bool
	mergeEnc   encryptFlags
)

// exitCodeFor maps a merge or split failure to an exit code: 2 for an unreadable or corrupt input bundle, 3 otherwise.
func exitCodeFor(err error) int {
	var inErr *trace.InputError
	if errors.As(err, &inErr) {
		return 2
	}
	return 3
}

// refuseInputOverwrite reports an error when out is the same file as one of the inputs.
func refuseInputOverwrite(out string, inputs []string) error {
	outInfo, err := os.Stat(out)
	if err != nil {
		return nil //nolint:nilerr // a missing output cannot be an input
	}
	for _, in := range inputs {
		if info, err := os.Stat(in); err == nil && os.SameFile(outInfo, info) {
			return fmt.Errorf("refusing to overwrite input %s", in)
		}
	}
	return nil
}

func runMerge(inputs []string) int {
	if mergeOut == "" {
		fmt.Fprintln(os.Stderr, "Error: --out is required")
		return 3
	}
	exportOpts, err := mergeEnc.exportOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}
	importOpts, err := importOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}
	if err := refuseInputOverwrite(mergeOut, inputs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}
	if _, err := os.Lstat(mergeOut); err == nil && !mergeForce {
		fmt.Fprintf(os.Stderr, "Error: %s already exists (use --force to overwrite)\n", mergeOut)
		return 3
	}

	stats, err := trace.MergeFiles(mergeOut, inputs, importOpts, exportOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return exitCodeFor(err)
	}
	fmt.Printf("merged %d runs into %s (%d objects, %d shared)\n", stats.Runs, mergeOut, stats.Objects, stats.Shared)
	return 0
}

var mergeCmd = &cobra.Command{
	Use:   "merge -o <out.zip> <bundle-or-archive>...",
	Short: "Merge bundles into one multi-run archive",
	Long: `Combine single-run bundles and multi-run archives into one multi-run archive (at least 2 runs in total).
The result is a corpus in a single file: "zyg verify --corpus <out.zip>" replays every run in it.

Layout of the archive (a zip):

  runs.json                 {"version":1,"format":"zygote-multi-run","runs":[{"name":"<run>","manifest":"runs/<run>/trace.json"}, ...]}
  runs/<run>/trace.json     the run's recording, the exact bytes of its original trace.json
  objects/<blake3 hex>      one shared object store; identical objects are stored once

There is no top-level trace.json, so older zyg versions refuse the archive instead of misreading it. The same inputs always
produce the same bytes. A single-run input is named after its file name without .zip; a multi-run input keeps its run names.
Names must match [A-Za-z0-9._-]+ (not "." or ".."), at most 128 characters, and be unique; a duplicate is an error naming both inputs.
On success it prints: merged N runs into <out> (M objects, K shared), where K counts objects used by more than one run.

It fails closed: every input is fully validated first (object hashes, reachability of every step and CAS-stored effect value, manifest
versions), and on any error nothing is written. The output is written atomically. It refuses to overwrite an input, and refuses to
overwrite an existing -o file unless --force is given.

Encrypted inputs are opened with --identity-file / ZYG_IDENTITY_FILE or ZYG_PASSPHRASE_FILE / ZYG_PASSPHRASE; one key applies to all
inputs. The output is encrypted only with --encrypt, using the same flags as "zyg record" (--recipient, --recipients-file, --passphrase).

Exit codes: 2 an input is unreadable, corrupt or cannot be decrypted; 3 usage or output error.`,
	Args: cobra.MinimumNArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		os.Exit(runMerge(args))
	},
}

func init() {
	mergeCmd.Flags().StringVarP(&mergeOut, "out", "o", "", "Output archive file")
	mergeCmd.Flags().BoolVar(&mergeForce, "force", false, "Overwrite the output file if it exists")
	mergeEnc.register(mergeCmd)
	rootCmd.AddCommand(mergeCmd)
}
