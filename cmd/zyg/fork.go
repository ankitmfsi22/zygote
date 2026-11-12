package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/mindfire/zygote/source/localdir"
	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var forkInto string

var forkCmd = &cobra.Command{
	Use:   "fork [run.zip] [step]",
	Short: "Materialize a step into a local directory",
	Args:  cobra.ExactArgs(2),
	Run: func(_ *cobra.Command, args []string) {
		bundlePath := args[0]
		stepStr := args[1]

		stepNum, err := strconv.Atoi(stepStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid step number: %v\n", err)
			os.Exit(3)
		}

		if forkInto == "" {
			fmt.Fprintf(os.Stderr, "Error: --into is required\n")
			os.Exit(3)
		}

		store := vfs.NewMemStore()
		bundle, err := loadBundle(bundlePath, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading bundle: %v\n", err)
			os.Exit(2)
		}

		if stepNum < 0 || stepNum >= len(bundle.Recording.Steps) {
			fmt.Fprintf(os.Stderr, "Step out of range (max %d)\n", len(bundle.Recording.Steps)-1)
			os.Exit(3)
		}

		step := bundle.Recording.Steps[stepNum]
		rootHex := step.Root

		hBytes, err := hex.DecodeString(rootHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid root hash in bundle: %v\n", err)
			os.Exit(2)
		}

		var h vfs.Hash
		copy(h[:], hBytes)

		world := vfs.Fork(store, vfs.Snapshot{Root: h})

		adapter := localdir.New(forkInto)
		if err := adapter.Apply(context.Background(), world); err != nil {
			fmt.Fprintf(os.Stderr, "Error exporting world to %s: %v\n", forkInto, err)
			os.Exit(3)
		}

		fmt.Printf("Successfully materialised step %d (%s) into %s\n", stepNum, step.Name, forkInto)
	},
}

func init() {
	forkCmd.Flags().StringVar(&forkInto, "into", "", "Directory to materialise the world into")
	rootCmd.AddCommand(forkCmd)
}
