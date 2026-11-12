// Package main provides the CLI for Zygote.
package main

import (
	"fmt"
	"os"

	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var stepsCmd = &cobra.Command{
	Use:   "steps [run.zip]",
	Short: "List steps in a recording",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		bundlePath := args[0]
		store := vfs.NewMemStore()

		bundle, err := loadBundle(bundlePath, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(2) // bundle invalid
		}

		fmt.Printf("%-5s %-20s %-12s %s\n", "STEP", "NAME", "HASH", "EFFECTS")
		for _, s := range bundle.Recording.Steps {
			hashStr := s.Root
			if len(hashStr) > 8 {
				hashStr = hashStr[:8]
			}
			fmt.Printf("%-5d %-20s %-12s %d\n", s.N, s.Name, hashStr, s.Effects)
		}
	},
}

func init() {
	rootCmd.AddCommand(stepsCmd)
}
