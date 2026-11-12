package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/pkg/redact"
	"github.com/mindfire/zygote/source/localdir"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var (
	recordAgent string
	recordDir   string
	recordOut   string

	recordAutoStep bool
	recordEnc      encryptFlags
)

func runRecord() int {
	if recordAgent == "" {
		fmt.Fprintln(os.Stderr, "Error: --agent is required")
		return 3
	}

	exportOpts, err := recordEnc.exportOptions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 3
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for SIGINT and SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, finalizing recording...")
		cancel()
	}()

	store := vfs.NewMemStore()
	var opts []trace.Option
	if recordAutoStep {
		opts = append(opts, trace.WithAutoStep())
	}
	r := trace.Record(store, opts...)

	adapter := localdir.New(recordDir)
	if err := adapter.Load(ctx, r.World()); err != nil {
		fmt.Fprintf(os.Stderr, "Error loading directory: %v\n", err)
		return 3
	}
	r.Step("init")

	cmd := exec.CommandContext(ctx, "sh", "-c", recordAgent) //nolint:gosec

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating stdout pipe: %v\n", err)
		return 3
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating stdin pipe: %v\n", err)
		return 3
	}

	cmd.Stderr = os.Stderr
	cmd.Dir = recordDir
	cmd.Env = agentEnv()

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting agent: %v\n", err)
		return 3
	}

	srv := harness.NewServer(stdoutPipe, stdinPipe, harness.NewRecordHandler(r))

	srvErr := srv.Serve()

	cmdErr := cmd.Wait()

	exitCode := 0
	if srvErr != nil {
		fmt.Fprintf(os.Stderr, "Harness protocol error (or agent stopped): %v\n", srvErr)
		exitCode = 3
	} else if cmdErr != nil && cmdErr.Error() != "signal: killed" {
		fmt.Fprintf(os.Stderr, "Agent exited with error: %v\n", cmdErr)
		exitCode = 3
	}

	r.Finish()

	// Always save the bundle (NFR-3.6)
	bundle := &trace.Bundle{
		Recording: r.Recording(),
		Store:     store,
	}

	policyPath := filepath.Join(recordDir, ".zygote", "redact.yaml")
	policy, err := redact.LoadPolicy(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading redact policy: %v\n", err)
		return 3
	}

	cleanBundle, err := trace.RedactBundle(bundle, policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Redaction failed (fail-closed): %v\n", err)
		return 3
	}

	if err := trace.ExportFile(recordOut, cleanBundle.Store, cleanBundle.Recording, exportOpts...); err != nil {
		fmt.Fprintf(os.Stderr, "Error exporting bundle: %v\n", err)
		return 3
	}

	fmt.Printf("Successfully recorded run to %s\n", recordOut)
	return exitCode
}

var recordCmd = &cobra.Command{
	Use:   "record",
	Short: "Record an agent run",
	Run: func(_ *cobra.Command, _ []string) {
		os.Exit(runRecord())
	},
}

func init() {
	recordCmd.Flags().StringVar(&recordAgent, "agent", "", "Agent command to run")
	recordCmd.Flags().StringVar(&recordDir, "dir", ".", "Directory to record")
	recordEnc.register(recordCmd)
	recordCmd.Flags().StringVarP(&recordOut, "out", "o", "run.zip", "Output bundle file")
	recordCmd.Flags().BoolVar(&recordAutoStep, "auto-step", false, "Derive steps automatically from tool effects")
	rootCmd.AddCommand(recordCmd)
}
