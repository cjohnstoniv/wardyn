// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-runnerd serves a registered local Docker runner over its
// outbound, authenticated organisation connection.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := rootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-runnerd:", err)
		os.Exit(1)
	}
}

func rootCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "wardyn-runnerd", Short: "Run a registered Wardyn runner", Version: version.String(), SilenceUsage: true, SilenceErrors: true}
	cmd.AddCommand(newRegisterCommand(), newServeCommand(productionRunnerHooks))
	return cmd
}
