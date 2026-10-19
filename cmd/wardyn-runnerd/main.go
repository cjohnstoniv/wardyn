// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-runnerd serves a registered local Docker runner over its
// outbound, authenticated organisation connection.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/nodump"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

func main() {
	// First, before any run's credentials arrive: no core dump, no same-uid ptrace.
	if err := nodump.Disable(); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-runnerd:", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := rootCommand().ExecuteContext(ctx)
	code := exitCode(ctx, err)
	if code != 0 {
		fmt.Fprintln(os.Stderr, "wardyn-runnerd:", err)
	}
	os.Exit(code)
}

// exitCode treats the cancellation a SIGTERM or interrupt causes as a clean stop,
// so an orchestrator does not read a deliberate shutdown as a crash.
func exitCode(ctx context.Context, err error) int {
	if err == nil || (errors.Is(err, context.Canceled) && ctx.Err() != nil) {
		return 0
	}
	return 1
}

func rootCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "wardyn-runnerd", Short: "Run a registered Wardyn runner", Version: version.String(), SilenceUsage: true, SilenceErrors: true,
		// Every command that talks to Docker follows the engine the docker CLI would use (resolveDockerHost);
		// the Docker SDK reads DOCKER_HOST alone.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			home, _ := os.UserHomeDir()
			host, err := resolveDockerHost(os.Getenv, home)
			if err != nil {
				return err
			}
			if host != "" && host != os.Getenv("DOCKER_HOST") {
				return os.Setenv("DOCKER_HOST", host)
			}
			return nil
		}}
	cmd.AddCommand(newRegisterCommand(), newSetupCommand(), newServeCommand(productionRunnerHooks))
	return cmd
}
