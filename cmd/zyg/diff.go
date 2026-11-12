package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

var diffCmd = &cobra.Command{
	Use:   "diff [run.zip] [stepA] [stepB]",
	Short: "Show what changed between two steps",
	Args:  cobra.ExactArgs(3),
	Run: func(_ *cobra.Command, args []string) {
		bundlePath := args[0]

		stepA, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid step A: %v\n", err)
			os.Exit(3)
		}

		stepB, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid step B: %v\n", err)
			os.Exit(3)
		}

		store := vfs.NewMemStore()
		bundle, err := loadBundle(bundlePath, store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading bundle: %v\n", err)
			os.Exit(2)
		}

		if stepA < 0 || stepA >= len(bundle.Recording.Steps) || stepB < 0 || stepB >= len(bundle.Recording.Steps) {
			fmt.Fprintf(os.Stderr, "Step out of range (max %d)\n", len(bundle.Recording.Steps)-1)
			os.Exit(3)
		}

		sa := bundle.Recording.Steps[stepA]
		sb := bundle.Recording.Steps[stepB]

		hA, _ := hex.DecodeString(sa.Root)
		hB, _ := hex.DecodeString(sb.Root)

		var rootA, rootB vfs.Hash
		copy(rootA[:], hA)
		copy(rootB[:], hB)

		changes, err := vfs.Diff(store, vfs.Snapshot{Root: rootA}, vfs.Snapshot{Root: rootB})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error computing diff: %v\n", err)
			os.Exit(3)
		}

		fmt.Printf("Comparing step %d (%s) -> step %d (%s):\n", stepA, sa.Name, stepB, sb.Name)

		if len(changes) == 0 {
			fmt.Println("No files changed.")
			return
		}

		for _, c := range changes {
			var k string
			switch c.Kind {
			case vfs.Added:
				k = "[ADDED]   "
			case vfs.Deleted:
				k = "[DELETED] "
			case vfs.Modified:
				k = "[MODIFIED]"
			}
			fmt.Printf("%s %s\n", k, c.Path)
		}
	},
}

func init() {
	rootCmd.AddCommand(diffCmd)
}
