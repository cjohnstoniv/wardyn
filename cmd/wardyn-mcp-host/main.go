// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-mcp-host runs one local (stdio) MCP server inside its own MCP
// sandbox and serves it over streamable HTTP to that sandbox's proxy, which is
// the only thing that can reach it. Before the server starts it writes the CA
// files the sandbox's trust variables name; it starts the server from
// WARDYN_MCP_LAUNCH_JSON with no shell, and it exits when the server does.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	code, err := run(ctx, options{
		caDir:   caDir,
		bundles: systemBundles,
		listen:  listenAddr,
		env:     os.Getenv,
		stderr:  os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-mcp-host:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
