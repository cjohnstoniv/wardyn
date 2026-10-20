// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// folder.go — `wardyn folder bring-back <run>`: end a live "Both ways" run whose folder came
// from your computer, bringing the sandbox's changes back to that folder first
// (POST /runs/{id}/computer-folder/bring-back).

// folderCopy is every line `wardyn folder` prints, in one table.
var folderCopy = struct {
	short, bringBackUse, bringBackShort, bringBackLong string
	progress, broughtBack, deleted, notDeleted         string
	leftOut, notBack                                   string
}{
	short:          "A folder from your computer on a run",
	bringBackUse:   "bring-back <run-id>",
	bringBackShort: "End a run and bring its folder back to your computer first",
	bringBackLong: "End a live run whose folder came from your computer (Both ways), bringing the sandbox's changes back first, " +
		"under the wardyn sync --pull rules: a file changed on both sides keeps your computer's copy, and nothing on your computer is deleted. " +
		"Only the run's own person can do it, and the agent is stopped first. A kill does not bring the folder back.",
	progress:    "Bringing changes back…\n",
	broughtBack: "Last changes synced back at %s. %d files came back to you.\n",
	deleted:     "The sandbox copy was deleted when the run ended.\n",
	notDeleted:  "The sandbox copy could not be deleted yet. Wardyn tries again within about two hours, and the next time the server starts.\n",
	leftOut:     "left out: %s (%s)\n",
	notBack:     "the run is stopped, but its folder did not come back: %s",
}

// folderBringBackTimeout covers the server's pass (2 minutes) and the teardown after it.
const folderBringBackTimeout = 5 * time.Minute

func folderCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{Use: "folder", Short: folderCopy.short}
	cmd.AddCommand(&cobra.Command{
		Use:   folderCopy.bringBackUse,
		Short: folderCopy.bringBackShort,
		Long:  folderCopy.bringBackLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			c := client()
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost,
				strings.TrimRight(c.BaseURL, "/")+"/api/v1/runs/"+id.String()+"/computer-folder/bring-back", nil)
			if err != nil {
				return err
			}
			if c.Token != "" {
				req.Header.Set("Authorization", "Bearer "+c.Token)
			}
			req.Header.Set("Accept", "application/json")
			fmt.Fprint(cmd.ErrOrStderr(), folderCopy.progress)
			hc := *rawHTTPClient(c)
			hc.Timeout = folderBringBackTimeout
			resp, err := hc.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return sdk.NewAPIError(resp.StatusCode, body)
			}
			var out struct {
				SandboxRemoved bool `json:"sandbox_removed"`
				Back           *struct {
					At      time.Time `json:"at"`
					Files   int       `json:"files"`
					Bytes   int64     `json:"bytes"`
					Error   string    `json:"error"`
					Refused []struct {
						Path   string `json:"path"`
						Reason string `json:"reason"`
					} `json:"refused"`
				} `json:"back"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				return err
			}
			if out.Back == nil {
				return fmt.Errorf(folderCopy.notBack, "no pass ran")
			}
			for _, r := range out.Back.Refused {
				fmt.Fprintf(cmd.OutOrStdout(), folderCopy.leftOut, r.Path, r.Reason)
			}
			if out.Back.Error != "" {
				return fmt.Errorf(folderCopy.notBack, out.Back.Error)
			}
			fmt.Fprintf(cmd.OutOrStdout(), folderCopy.broughtBack, out.Back.At.Local().Format("15:04"), out.Back.Files)
			// The folder came back either way; a teardown that failed is said, not hidden.
			if !out.SandboxRemoved {
				fmt.Fprint(cmd.ErrOrStderr(), folderCopy.notDeleted)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), folderCopy.deleted)
			return nil
		},
	})
	return cmd
}
