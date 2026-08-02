// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// secretCmd manages named secrets in the control plane's store. Values are
// write-only: the API never returns them (reads happen only inside the broker
// and the proxy injection-resolve path, both audited).
func secretCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage platform secrets (write-only; values are never readable back)",
	}

	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret (value read from stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stdin is the ONLY input: piped, redirected from a file, or a
			// prompted line. There is deliberately no --value flag — argv is
			// world-readable in `ps` / /proc/<pid>/cmdline, and this is the write
			// path for every platform secret (SSH private keys, git PATs, LLM API
			// keys, Bedrock credentials). Same call `subscription connect` makes.
			if isTerminal(os.Stdin) {
				fmt.Fprintf(os.Stderr, "value for %q — type it, then press Ctrl-D on a new line to finish (input is NOT hidden; prefer piping): ", args[0])
			}
			v, err := readSecretValue(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read value from stdin: %w", err)
			}
			if v == "" {
				return fmt.Errorf("empty secret value")
			}
			if err := client().SetSecret(cmd.Context(), args[0], v); err != nil {
				return err
			}
			fmt.Printf("secret %q stored\n", args[0])
			return nil
		},
	}

	var asJSON bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List secret names (never values)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, err := client().ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(names)
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "emit the names as a JSON array")

	del := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client().DeleteSecret(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("secret %q deleted\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(set, list, del)
	return cmd
}

// readSecretValue reads an entire secret value from r. Secrets are frequently
// multi-line (PEM private keys, JSON service-account blobs, multi-line tokens),
// so we must read ALL of stdin rather than a single line. The previous
// implementation used bufio.ReadString('\n') and silently truncated everything
// after the first newline, corrupting multi-line secrets (HIGH finding).
//
// We strip at most ONE trailing newline (and an accompanying CR) — the common
// artifact of an echoed prompt or a shell heredoc — but preserve all internal
// newlines and any other surrounding whitespace verbatim.
func readSecretValue(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	s := string(b)
	// Trim a single trailing "\r\n" or "\n", nothing more.
	if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSuffix(s, "\r")
	}
	return s, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
