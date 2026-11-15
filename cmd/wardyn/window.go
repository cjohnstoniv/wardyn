// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// windowCmd is `wardyn window <run>`: it asks the server to open the attended window onto a macOS VM run on
// your own Mac. The server decides and words every refusal; the CLI prints that sentence and exits non-zero.
func windowCmd(client clientFn) *cobra.Command {
	var status bool
	cmd := &cobra.Command{
		Use:   "window <run-id>",
		Short: "Open a window on a macOS VM run, on your own Mac",
		Long: "Opens a keyboard-and-pointer window onto a running macOS VM run, drawn on the Mac the run is on. " +
			"Only the run's owner can, and only on a Mac whose runner you registered. Closing the window hands the screen back to the run; the run goes on. " +
			"With --status, shows whether a window can open now and why your last one closed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if status {
				v, err := client().GetMacWindow(cmd.Context(), id)
				if err != nil {
					return sentenceError(err)
				}
				if v == nil {
					fmt.Fprintln(out, "This run has no window: only a running macOS VM run does.")
					return nil
				}
				fmt.Fprintf(out, "window: %s\n", v.State)
				if v.Message != "" {
					fmt.Fprintln(out, v.Message)
				}
				if lc := v.LastClose; lc != nil {
					fmt.Fprintf(out, "last window closed: %s (%s)\n", lc.Reason, lc.At.Format("2006-01-02 15:04:05 MST"))
				}
				return nil
			}
			g, err := client().OpenMacWindow(cmd.Context(), id)
			if err != nil {
				return sentenceError(err)
			}
			where := g.Runner
			if where == "" {
				where = "your Mac"
			}
			fmt.Fprintf(out, "The window is opening on %s. Closing it hands the screen back to the run; the run goes on.\n", where)
			return nil
		},
	}
	cmd.Flags().BoolVar(&status, "status", false, "show whether a window can open now and why your last one closed, instead of opening one")
	return cmd
}

// sentenceError turns a server refusal into its own sentence, with no status prefix around it; the result
// still unwraps to the refusal, so the exit code keeps its class. Anything that is not a refusal with a
// sentence passes through unchanged.
func sentenceError(err error) error {
	var ae *sdk.APIError
	if !errors.As(err, &ae) {
		return err
	}
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(ae.Body), &env) != nil || env.Error == "" {
		return err
	}
	return &sentenceAPIError{sentence: env.Error, err: err}
}

// sentenceAPIError is a refusal shown as the server's sentence alone.
type sentenceAPIError struct {
	sentence string
	err      error
}

func (e *sentenceAPIError) Error() string { return e.sentence }
func (e *sentenceAPIError) Unwrap() error { return e.err }
