// Package main provides the CLI for Zygote.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var (
	inspectEffects bool
	inspectMeta    bool
)

var inspectCmd = &cobra.Command{
	Use:   "inspect [run.zip]",
	Short: "Inspect bundle metadata and effects",
	Args:  cobra.ExactArgs(1),
	Run: func(_ *cobra.Command, args []string) {
		bundlePath := args[0]
		store := vfs.NewMemStore()

		bundle, err := loadBundle(bundlePath, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(2)
		}

		if inspectMeta {
			fmt.Println("=== Metadata ===")
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(bundle.Recording.Meta)
			fmt.Println()
		}

		if inspectEffects {
			fmt.Println("=== Effects ===")
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(bundle.Recording.Effects)
		}

		if !inspectMeta && !inspectEffects {
			fmt.Println("Use --meta to see metadata or --effects to see the effect log.")
		}
	},
}

func init() {
	inspectCmd.Flags().BoolVar(&inspectEffects, "effects", false, "Print the effect log")
	inspectCmd.Flags().BoolVar(&inspectMeta, "meta", false, "Print metadata")
	rootCmd.AddCommand(inspectCmd)
}
