// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// `wardyn drive` — the offboarding half of user drives that is NOT a console
// screen (#166).
//
// One verb, and it destroys data: `reclaim` deletes the storage object one
// person's allocation resolved to, on whichever substrate this deployment
// dispatches. There is no console button for it in 0.8 — a destructive
// confirmation is a screen, and this one has no approved mock — so the API and
// this command are the whole surface.
//
// RAW HTTP RATHER THAN AN SDK METHOD, deliberately. `/api/v1/drives` is on the
// NOT-covered half of pkg/client's route-family census (see client.go's
// doctrine block): the family is admin-only and authored through the console,
// and wrapping one verb of it would move the whole family across a line two
// census guards hold. `mintAttachTicket` in attach.go reaches the other
// deliberately-unwrapped route the CLI needs the same way.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// driveCmd returns `wardyn drive`.
func driveCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive",
		Short: "Operate on user drives (reclaim destroys one person's storage)",
	}
	cmd.AddCommand(driveReclaimCmd(client))
	return subcommandGroup(cmd)
}

// driveReclaimResult is `drive reclaim`'s output — the same seven facts the
// `drive.reclaim` audit row carries, so what an operator pastes into a ticket
// and what the trail records cannot describe the act differently.
type driveReclaimResult struct {
	Drive       string    `json:"drive"`
	DriveID     uuid.UUID `json:"drive_id"`
	SubjectType string    `json:"subject_type"`
	Subject     string    `json:"subject"`
	Backend     string    `json:"backend"`
	Object      string    `json:"object"`
	Outcome     string    `json:"outcome"`
}

func driveReclaimCmd(client clientFn) *cobra.Command {
	var subject, subjectType string
	var yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "reclaim <drive-id>",
		Short: "DESTROY the storage one person's drive allocation resolved to",
		Long: `Delete the substrate object — a Docker volume or a PersistentVolumeClaim —
that one person's allocation of a drive resolved to.

THIS DELETES DATA AND NOTHING UNDOES IT. Run it against the object name the
drive preview prints, and run it BEFORE deleting the allocation: with the
allocation gone, a home directory an admin pinned (home_override) can no longer
be recovered from the database and the name this command derives is the drive
template's.

Super-admin only. Refused with a 409 while a run still holds the object, or
while the object answering to that name is not this drive's. Every attempt —
successes, refusals and failures alike — is audited as ` + "`drive.reclaim`" + `.

On Kubernetes the daemon holds no delete verb on claims unless the chart's
userDrives.reclaim.enabled is set, so on a stock install every attempt fails
with the apiserver's own 403. See docs/OPERATIONS.md.

The outcome is 'deleted' (this call destroyed the storage) or 'already_absent'
(nothing answered to the name, so nothing was destroyed).
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDriveReclaim(cmd, client(), args[0], subjectType, subject, yes, asJSON)
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "the person's sign-in subject (required)")
	cmd.Flags().StringVar(&subjectType, "subject-type", "user", "subject type; only `user` names one object to destroy")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm that this permanently destroys the person's stored data")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {drive, drive_id, subject_type, subject, backend, object, outcome} as JSON")
	return cmd
}

// runDriveReclaim posts the destroy verb.
//
// --yes is required rather than prompted: this runs in offboarding scripts as
// often as at a keyboard, and a prompt that only appears on a terminal is a
// guard the automated path silently loses. Asking for the flag makes the
// deliberate half explicit in the command that gets reviewed.
func runDriveReclaim(cmd *cobra.Command, c *sdk.Client, id, subjectType, subject string, yes, asJSON bool) error {
	driveID, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("drive reclaim: %q is not a drive id (a uuid); `wardyn drive reclaim <drive-id>`", id)
	}
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("drive reclaim: --subject is required — a reclaim names ONE person's storage")
	}
	if !yes {
		return fmt.Errorf("drive reclaim: refusing without --yes: this permanently destroys the storage "+
			"allocated to %s on drive %s, and nothing undoes it", subject, driveID)
	}
	body, err := json.Marshal(map[string]string{"subject_type": subjectType, "subject": subject})
	if err != nil {
		return fmt.Errorf("drive reclaim: encode request: %w", err)
	}
	res, err := postDriveReclaim(cmd.Context(), c, driveID, body)
	if err != nil {
		return err
	}
	if asJSON {
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return fmt.Errorf("drive reclaim: encode result: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s) on drive %q for %s\n",
		res.Outcome, res.Object, res.Backend, res.Drive, res.Subject)
	return nil
}

// postDriveReclaim is the one raw call, shaped like mintAttachTicket's: any
// non-2xx becomes *sdk.APIError so main()'s exit-code mapping and the error
// line read exactly as they do for every wrapped call. Unlike that one there
// is NO inconclusive arm — a destructive verb has no fallback path, so every
// answer that is not a 2xx is surfaced.
func postDriveReclaim(ctx context.Context, c *sdk.Client, driveID uuid.UUID, body []byte) (driveReclaimResult, error) {
	target := strings.TrimRight(c.BaseURL, "/") + "/api/v1/drives/" + driveID.String() + "/reclaim"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return driveReclaimResult{}, fmt.Errorf("drive reclaim: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return driveReclaimResult{}, fmt.Errorf("drive reclaim: %w", err)
	}
	defer resp.Body.Close()

	// The same 2 KiB cap pkg/client.maxErrBody uses, kept local for the same
	// reason attach.go keeps its own.
	const maxReclaimBody = 2048
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReclaimBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return driveReclaimResult{}, &sdk.APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out driveReclaimResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return driveReclaimResult{}, fmt.Errorf("drive reclaim: decode response: %w", err)
	}
	return out, nil
}
