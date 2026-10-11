// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// secretCmd manages named secrets in the control plane's store. Values are
// write-only: the API never returns them (reads happen only inside the broker
// and the proxy injection-resolve path, both audited).
func secretCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage platform secrets (write-only; values are never readable back)",
	}

	var personal, shared bool
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret (value read from stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stdin is the ONLY input: piped, redirected from a file, or a
			// prompted line. There is deliberately no --value flag — argv is
			// world-readable in `ps` / /proc/<pid>/cmdline, and this is the write
			// path for every platform secret (SSH private keys, git PATs).
			if isTerminal(os.Stdin) {
				fmt.Fprintf(cmd.ErrOrStderr(), "value for %q — type it, then press Ctrl-D on a new line to finish (input is NOT hidden; prefer piping): ", args[0])
			}
			v, err := readSecretValue(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read value from stdin: %w", err)
			}
			if v == "" {
				return fmt.Errorf("empty secret value")
			}
			put := client().SetSecret
			switch {
			case personal && shared:
				return fmt.Errorf("--personal and --shared are exclusive")
			case personal:
				put = client().SetMySecret
			case shared:
				put = client().SetSharedSecret
			}
			if err := put(cmd.Context(), args[0], v); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret %q stored\n", args[0])
			return nil
		},
	}
	set.Flags().BoolVar(&personal, "personal", false, "store it in your own namespace, whatever your role")
	set.Flags().BoolVar(&shared, "shared", false, "store it as a shared service secret (operator tier)")

	var asJSON, listPersonal, listShared bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List secret names (never values)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if listPersonal || listShared {
				if listPersonal && listShared {
					return fmt.Errorf("--personal and --shared are exclusive")
				}
				list := client().ListSharedSecrets
				if listPersonal {
					list = client().ListMySecrets
				}
				return printNamespaceNames(cmd, list, asJSON)
			}
			names, mine, err := client().ListSecretsScoped(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				// The server's object shape, not a flattened array: `names` and
				// `mine` are independent lists (see ListSecretsScoped) and a
				// member's own secrets would otherwise be indistinguishable from
				// the operator-owned ones they can also see.
				return emitJSON(cmd.OutOrStdout(), map[string]any{"names": names, "mine": mine})
			}
			printSecretScopes(cmd.OutOrStdout(), names, mine)
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "emit {\"names\":[...],\"mine\":[...]} (the server's shape; {\"names\":[...]} with --personal or --shared)")
	list.Flags().BoolVar(&listPersonal, "personal", false, "list only the names in your own namespace, whatever your role")
	list.Flags().BoolVar(&listShared, "shared", false, "list only the shared service secrets (operator tier)")

	var delPersonal, delShared bool
	del := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			remove := client().DeleteSecret
			switch {
			case delPersonal && delShared:
				return fmt.Errorf("--personal and --shared are exclusive")
			case delPersonal:
				remove = client().DeleteMySecret
			case delShared:
				remove = client().DeleteSharedSecret
			}
			if err := remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret %q deleted\n", args[0])
			return nil
		},
	}

	del.Flags().BoolVar(&delPersonal, "personal", false, "delete from your own namespace, whatever your role")
	del.Flags().BoolVar(&delShared, "shared", false, "delete a shared service secret (operator tier)")

	cmd.AddCommand(set, list, del)
	return subcommandGroup(cmd)
}

// readSecretValue reads an entire secret value from r. Secrets are frequently
// multi-line (PEM private keys, JSON service-account blobs, multi-line tokens),
// so this reads ALL of stdin rather than a single line: stopping at the first
// newline would silently truncate and corrupt a multi-line secret.
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

// printSecretScopes prints the union of names and mine, one per line, each
// marked with the namespace(s) it appears in. For an operator caller the two
// lists are identical (server-side invariant: mine == names), so every line
// reads "(operator, mine)"; for a member they are normally disjoint — the
// operator-owned names a grant pairs with vs. the member's own rows — so
// each line reads one or the other. The two lists are combined here (rather
// than printed as two blocks) so the output stays a flat, greppable list of
// names either way.
func printSecretScopes(w io.Writer, names, mine []string) {
	isMine := make(map[string]bool, len(mine))
	for _, n := range mine {
		isMine[n] = true
	}
	seen := make(map[string]bool, len(names)+len(mine))
	all := append(append([]string{}, names...), mine...)
	slices.Sort(all)
	for _, n := range all {
		if seen[n] {
			continue
		}
		seen[n] = true
		scopes := "operator"
		switch {
		case isMine[n] && slices.Contains(names, n):
			scopes = "operator, mine"
		case isMine[n]:
			scopes = "mine"
		}
		fmt.Fprintf(w, "%s\t(%s)\n", n, scopes)
	}
}

// printNamespaceNames is `secret list --personal` or `--shared`: one namespace, named by its own route.
func printNamespaceNames(cmd *cobra.Command, list func(context.Context, ...sdk.ListOpts) ([]string, error), asJSON bool) error {
	names, err := list(cmd.Context())
	if err != nil {
		return err
	}
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"names": names})
	}
	for _, n := range names {
		fmt.Fprintln(cmd.OutOrStdout(), n)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
