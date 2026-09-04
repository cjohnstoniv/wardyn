// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// runRecordingCmd downloads a run's terminal recording. It lives under the
// `run` noun, NOT under `wardyn record` — that noun is Recording MODE (learning
// a least-privilege policy from a run's activity), an unrelated concept the
// name would fuse with this one.
func runRecordingCmd(client clientFn) *cobra.Command {
	var outPath, session string
	rec := &cobra.Command{
		Use:   "recording <run-id>",
		Short: "Download a run's terminal recording as an asciicast (stdout unless -o)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			// W21-S1-6: defaults to the run's own (bare-id) cast when --session is
			// unset — an interactive run's OTHER recordings (one per attach
			// session, keyed "<run-id>~<session>") are otherwise unreachable from
			// the CLI/SDK even though the server has always served them.
			rc, err := client().GetRecording(cmd.Context(), id, session)
			if err != nil {
				return err
			}
			defer rc.Close()
			if outPath == "" {
				_, err = io.Copy(os.Stdout, rc)
				return err
			}
			// Downloaded to a .part and renamed on success: a transfer that
			// dies partway (a dropped link, a killed server) otherwise left a
			// truncated cast at exactly the path a player — or the next step of
			// a CI job — then opens, and the only symptom was a parse error
			// much later somewhere else. The rename is atomic on the same
			// directory, so outPath either does not exist or is the whole file.
			partPath := outPath + ".part"
			f, err := os.Create(partPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, rc); err != nil {
				f.Close()
				os.Remove(partPath)
				return fmt.Errorf("recording download for run %s did not complete: %w", id, err)
			}
			// Close is checked: a swallowed flush error writes a truncated cast
			// that only fails much later, in a player.
			if err := f.Close(); err != nil {
				os.Remove(partPath)
				return fmt.Errorf("recording download for run %s did not complete: %w", id, err)
			}
			if err := os.Rename(partPath, outPath); err != nil {
				os.Remove(partPath)
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
			return nil
		},
	}
	rec.Flags().StringVarP(&outPath, "output", "o", "", "write the .cast here instead of stdout")
	rec.Flags().StringVar(&session, "session", "", "attach-session id, for an interactive run's OTHER recordings (default: the run's own recording)")
	return rec
}
