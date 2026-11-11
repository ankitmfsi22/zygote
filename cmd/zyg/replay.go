// Package main provides the CLI for Zygote.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/mindfire/zygote/harness"
	"github.com/mindfire/zygote/journal"
	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var (
	replayAgent string
	replayJSON  bool
)

type divergenceOutput struct {
	Status  string `json:"status"`
	Class   string `json:"class"`
	Step    int    `json:"step"`
	Message string `json:"message"`
}

var replayCmd = &cobra.Command{
	Use:   "replay [run.zip]",
	Short: "Replay an agent run from a bundle",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		if replayAgent == "" {
			fmt.Fprintln(os.Stderr, "Error: --agent is required")
			os.Exit(3)
		}

		bundlePath := args[0]
		store := vfs.NewMemStore()

		bundle, err := trace.ImportFile(bundlePath, store)
		if err != nil {
			if replayJSON {
				fmt.Printf(`{"status":"error","message":%q}`+"\n", err.Error())
			} else {
				fmt.Fprintf(os.Stderr, "Error importing bundle: %v\n", err)
			}
			os.Exit(2) // bundle invalid
		}

		r, err := trace.Replay(store, &bundle)
		if err != nil {
			if replayJSON {
				fmt.Printf(`{"status":"error","message":%q}`+"\n", err.Error())
			} else {
				fmt.Fprintf(os.Stderr, "Error initializing replay: %v\n", err)
			}
			os.Exit(2)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", replayAgent) //nolint:gosec

		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating stdout pipe: %v\n", err)
			os.Exit(3)
		}

		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating stdin pipe: %v\n", err)
			os.Exit(3)
		}

		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting agent: %v\n", err)
			os.Exit(3)
		}

		srv := harness.NewServer(stdoutPipe, stdinPipe, harness.NewReplayHandler(r))

		srvErr := srv.Serve()
		cancel()
		cmdErr := cmd.Wait()

		if srvErr == nil && (cmdErr == nil || cmdErr.Error() == "signal: killed") {
			srvErr = r.Finish()
		}

		if srvErr != nil {
			var divErr *journal.DivergenceError
			if errors.As(srvErr, &divErr) {
				if replayJSON {
					out := divergenceOutput{
						Status:  "diverged",
						Class:   string(divErr.Class),
						Step:    divErr.Step,
						Message: divErr.Message,
					}
					b, _ := json.Marshal(out)
					fmt.Println(string(b))
				} else {
					fmt.Fprintf(os.Stderr, "[!] DIVERGENCE DETECTED [%s] at step %d: %s\n", divErr.Class, divErr.Step, divErr.Message)
				}
				os.Exit(1) // Divergence
			}
			if replayJSON {
				fmt.Printf(`{"status":"error","message":%q}`+"\n", srvErr.Error())
			} else {
				fmt.Fprintf(os.Stderr, "Harness protocol error: %v\n", srvErr)
			}
			os.Exit(3)
		}

		if cmdErr != nil && cmdErr.Error() != "signal: killed" {
			if replayJSON {
				fmt.Printf(`{"status":"error","message":%q}`+"\n", cmdErr.Error())
			} else {
				fmt.Fprintf(os.Stderr, "Agent exited with error: %v\n", cmdErr)
			}
			os.Exit(3)
		}

		if replayJSON {
			fmt.Println(`{"status": "match"}`)
		} else {
			fmt.Println("Replay matched all steps perfectly.")
		}
	},
}

func init() {
	replayCmd.Flags().StringVar(&replayAgent, "agent", "", "Agent command to run")
	replayCmd.Flags().BoolVar(&replayJSON, "json", false, "Output machine-readable JSON")
	rootCmd.AddCommand(replayCmd)
}
