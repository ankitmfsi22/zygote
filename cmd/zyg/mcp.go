package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	zmcp "github.com/mindfire/zygote/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

var mcpRoot string

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the agent-facing zygote tools over MCP on stdio",
	Run: func(_ *cobra.Command, _ []string) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := zmcp.Serve(ctx, &mcpsdk.StdioTransport{}, zmcp.Options{Root: mcpRoot}); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
			stop()
			os.Exit(1)
		}
	},
}

func init() {
	mcpCmd.Flags().StringVar(&mcpRoot, "root", ".", "Directory that all tool paths must stay inside")
	rootCmd.AddCommand(mcpCmd)
}
