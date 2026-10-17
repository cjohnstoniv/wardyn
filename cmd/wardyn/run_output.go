// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// writerIsTerminal reports whether w is a terminal. A var so a test can wire
// a pty without a real process stdout.
var writerIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// runOutputCmd returns `wardyn run output <run-id>`: the kept output of a run,
// from the same endpoint the console reads.
func runOutputCmd(client clientFn) *cobra.Command {
	var tail int
	var asJSON, raw bool
	cmd := &cobra.Command{
		Use:   "output <run-id>",
		Short: "Print a run's kept output (the end of its stdout/stderr)",
		Long: `Print the end of a run's kept output, with its source and completeness.

Piped, the bytes are written exactly as the server returned them, with no
added newline. On a terminal, escape and control characters other than newline,
carriage return and tab are printed as escapes (such as \x1b), because a run
controls this text; --raw turns that off. A pane_snapshot (an interactive run's
last screen) is plain text and is always filtered on a terminal.

A note on stderr says when the output is truncated, incomplete, has a capture
gap, is a pane snapshot, or was masked against global secrets only. A refusal
exits non-zero and names its run_output_* reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			if tail < 0 {
				return fmt.Errorf("--tail must be a positive number of bytes")
			}
			out, err := client().RunOutput(cmd.Context(), id, tail)
			if err != nil {
				var apiErr *sdk.APIError
				if errors.As(err, &apiErr) && apiErr.Reason != "" {
					return fmt.Errorf("%s: %w", apiErr.Reason, err)
				}
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			if n := runOutputNotice(out); n != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), n)
			}
			text := out.Output
			if (!raw || out.Source == paneSnapshotSource) && writerIsTerminal(cmd.OutOrStdout()) {
				text = escapeControls(text)
			}
			_, err = io.WriteString(cmd.OutOrStdout(), text)
			return err
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 0, "read only the last N bytes (0 = all the server keeps)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the response body as JSON")
	cmd.Flags().BoolVar(&raw, "raw", false, "on a terminal, write the bytes unfiltered (a run can print terminal control sequences; a pane snapshot is always filtered)")
	return cmd
}

const paneSnapshotSource = "pane_snapshot"

// runOutputNotice is the one-line stderr note for a row that is not a plain
// complete capture; "" when there is nothing to say.
func runOutputNotice(o sdk.RunOutput) string {
	var parts []string
	if o.Truncated {
		parts = append(parts, "truncated: the output does not start at the run's first byte")
	}
	if o.Incomplete {
		parts = append(parts, "incomplete: some bytes may be missing")
	}
	if o.CaptureGap {
		parts = append(parts, "capture gap: the output could not be captured")
	}
	if o.Source == paneSnapshotSource {
		parts = append(parts, "pane snapshot: the run's last terminal screen as plain text")
	}
	if o.MaskScope == "globals_only" {
		parts = append(parts, "masked against global secrets only")
	}
	if len(parts) == 0 {
		return ""
	}
	return "note: " + strings.Join(parts, "; ")
}

// escapeControls writes ESC, C1 controls, other C0 controls (not \n, \r, \t)
// and bytes that are not valid UTF-8 as \xNN, so a terminal never interprets
// them.
func escapeControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r < 0x20 && r != '\n' && r != '\r' && r != '\t', r >= 0x80 && r <= 0x9f:
			b.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}
