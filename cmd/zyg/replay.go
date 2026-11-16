// Package main provides the CLI for Zygote.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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

func printReplayOutcome(o replayOutcome) int {
	switch o.Verdict {
	case verdictMatch:
		if replayJSON {
			fmt.Println(`{"status": "match"}`)
		} else {
			fmt.Println("Replay matched all steps perfectly.")
		}
		return 0
	case verdictDiverged:
		d := o.Divergence
		if replayJSON {
			b, _ := json.Marshal(divergenceOutput{
				Status:  "diverged",
				Class:   string(d.Class),
				Step:    d.Step,
				Message: d.Message,
			})
			fmt.Println(string(b))
		} else {
			fmt.Fprintf(os.Stderr, "[!] DIVERGENCE DETECTED [%s] at step %d: %s\n", d.Class, d.Step, d.Message)
		}
		return 1
	}

	format, jsonOK := "Error: %v\n", false
	switch o.Stage {
	case stageImport:
		format, jsonOK = "Error importing bundle: %v\n", true
	case stageInit:
		format, jsonOK = "Error initializing replay: %v\n", true
	case stagePipe:
		format = "Error %v\n"
	case stageStart:
		format = "Error starting agent: %v\n"
	case stageHarness:
		format, jsonOK = "Harness protocol error: %v\n", true
	case stageAgent:
		format, jsonOK = "Agent exited with error: %v\n", true
	}
	if replayJSON && jsonOK {
		fmt.Printf(`{"status":"error","message":%q}`+"\n", o.Err.Error())
	} else {
		fmt.Fprintf(os.Stderr, format, o.Err)
	}
	if o.Verdict == verdictInvalid {
		return 2
	}
	return 3
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
		os.Exit(printReplayOutcome(replayBundle(context.Background(), args[0], replayAgent, 0)))
	},
}

func init() {
	replayCmd.Flags().StringVar(&replayAgent, "agent", "", "Agent command to run")
	replayCmd.Flags().BoolVar(&replayJSON, "json", false, "Output machine-readable JSON")
	rootCmd.AddCommand(replayCmd)
}
