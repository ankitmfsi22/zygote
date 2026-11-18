package main

import (
	"fmt"
	"os"

	"github.com/mindfire/zygote/trace"
	"github.com/spf13/cobra"
)

var (
	splitOut   string
	splitRuns  []string
	splitForce bool
	splitEnc   encryptFlags
)

func runSplit(archive string) int {
	if splitOut == "" {
		fmt.Fprintln(os.Stderr, "Error: --out is required")
		return 3
	}
	exportOpts, err := splitEnc.exportOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}
	importOpts, err := importOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}

	written, err := trace.SplitFile(archive, splitOut, trace.SplitParams{Runs: splitRuns, Force: splitForce}, importOpts, exportOpts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return exitCodeFor(err)
	}
	fmt.Printf("split %d runs from %s into %s\n", len(written), archive, splitOut)
	return 0
}

var splitCmd = &cobra.Command{
	Use:   "split <archive.zip> -o <dir> [--run <name>]...",
	Short: "Split a multi-run archive into single-run bundles",
	Long: `Write each run of a multi-run archive (see "zyg merge") to <dir>/<run>.zip as a standard single-run bundle,
or only the runs named with --run (repeatable; an unknown name is an error). <dir> is created if needed.

Each output holds only the objects its run needs, and its trace.json is the run's original bytes, so it imports and replays exactly like
the bundle the run came from; an unencrypted bundle that was merged and split again is byte-identical to the original.

It fails closed: the archive is fully validated first (object hashes, reachability of every step and CAS-stored effect value, runs.json,
manifest versions) and on any error nothing is written. Outputs are staged and moved into place together. It refuses to overwrite an
existing file in <dir> unless --force is given. A single-run bundle is not an archive and is rejected.

An encrypted archive is opened with --identity-file / ZYG_IDENTITY_FILE or ZYG_PASSPHRASE_FILE / ZYG_PASSPHRASE. The outputs are encrypted
only with --encrypt, using the same flags as "zyg record" (--recipient, --recipients-file, --passphrase).

Exit codes: 2 the archive is unreadable, corrupt, cannot be decrypted or is a single-run bundle; 3 usage or output error.`,
	Args: cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		os.Exit(runSplit(args[0]))
	},
}

func init() {
	splitCmd.Flags().StringVarP(&splitOut, "out", "o", "", "Output directory")
	splitCmd.Flags().StringArrayVar(&splitRuns, "run", nil, "Run to extract (repeatable; default all runs)")
	splitCmd.Flags().BoolVar(&splitForce, "force", false, "Overwrite existing files in the output directory")
	splitEnc.register(splitCmd)
	rootCmd.AddCommand(splitCmd)
}
