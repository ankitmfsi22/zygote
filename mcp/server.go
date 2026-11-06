// Package mcp serves the zygote agent-facing tools over the Model Context Protocol.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxRead is the default cap in bytes on a single world.read response.
const DefaultMaxRead = 1 << 20

// Options configures the MCP server.
type Options struct {
	Root    string
	MaxRead int
}

// NewServer builds an MCP server exposing the ten zygote tools.
func NewServer(opts Options) (*mcpsdk.Server, error) {
	root := opts.Root
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("root %s is not a directory", abs)
	}
	maxRead := opts.MaxRead
	if maxRead <= 0 {
		maxRead = DefaultMaxRead
	}

	s := &session{root: filepath.Clean(abs), maxRead: maxRead}
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "zyg", Version: "v0.1.0"},
		&mcpsdk.ServerOptions{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))},
	)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.start", Description: "Begin a recording run, optionally seeding the world from a directory under the root."}, s.runStart)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.step", Description: "Close a named step and capture the current world hash."}, s.runStep)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.effect", Description: "Record one nondeterministic result the agent received."}, s.runEffect)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.input", Description: "Load input state into the world and close it as a named step."}, s.runInput)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "world.read", Description: "Read a file or list a directory from the live world or a recorded step."}, s.worldRead)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "world.write", Description: "Write or delete a file in the in-memory world."}, s.worldWrite)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "world.diff", Description: "Show file-level changes between two steps or the live world."}, s.worldDiff)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.fork", Description: "Materialise a recorded step into a directory under the root."}, s.runFork)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "run.export", Description: "Redact the run and write it as a bundle under the root."}, s.runExport)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "replay.verify", Description: "Verify a bundle's integrity and optionally compare step hashes against it."}, s.replayVerify)

	return srv, nil
}

// Serve runs the MCP server over the given transport until the client disconnects.
func Serve(ctx context.Context, t mcpsdk.Transport, opts Options) error {
	srv, err := NewServer(opts)
	if err != nil {
		return err
	}
	return srv.Run(ctx, t)
}
