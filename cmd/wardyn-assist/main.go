// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-assist runs inside a governed assistant run. `relay` carries turns between Wardyn and the
// Claude Code harness; `tools` is the stdio MCP server the harness starts for one session. Both reach Wardyn
// only through the run's proxy. Flag parsing and wiring only: the work is in internal/assistrelay.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistrelay"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: wardyn-assist relay|tools [flags]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	switch args[0] {
	case "relay":
		return relay(ctx, args[1:], stderr)
	case "tools":
		return tools(ctx, args[1:], stderr)
	}
	fmt.Fprintln(stderr, "usage: wardyn-assist relay|tools [flags]")
	return 2
}

func relay(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", os.Getenv("HTTP_PROXY"), "the run's proxy (default: $HTTP_PROXY)")
	mode := fs.String("mode", "resume", "harness mode: resume (one process per turn) or stream (one per session)")
	claude := fs.String("claude", "claude", "the harness command")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || (*mode != "resume" && *mode != "stream") {
		fmt.Fprintln(stderr, "wardyn-assist relay: -base is required and -mode is resume or stream")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "wardyn-assist relay: cannot find its own path")
		return assistrelay.ExitFailed
	}
	iso, err := assistrelay.NewIsolation("", assistrelay.FindCredential(os.Getenv))
	if err != nil {
		fmt.Fprintln(stderr, "wardyn-assist relay: cannot create the isolated directory")
		return assistrelay.ExitFailed
	}
	r := &assistrelay.Relay{
		API:      assistrelay.NewAPI(*base),
		Launcher: assistrelay.ExecLauncher{},
		Iso:      iso,
		Claude:   *claude,
		Self:     self,
		Env:      iso.Env(os.Environ()),
		Stream:   *mode == "stream",
		Log:      stderr,
	}
	return r.Run(ctx)
}

func tools(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "the session this process speaks for, fixed for its life")
	base := fs.String("base", os.Getenv("HTTP_PROXY"), "the run's proxy (default: $HTTP_PROXY)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := uuid.Parse(*session); err != nil || *base == "" {
		fmt.Fprintln(stderr, "wardyn-assist tools: -session (a uuid) and -base are required")
		return 2
	}
	s := &assistrelay.ToolServer{API: assistrelay.NewAPI(*base), Session: *session, Log: stderr}
	if err := s.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		return 1
	}
	return 0
}
