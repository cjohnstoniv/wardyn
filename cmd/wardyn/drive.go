// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// drive.go — read/replace the admin-registered drives and their allocations,
// the same get-then-set shape site-config.go established for the operator-
// wide baseline: `subcommandGroup`, `emitJSON`, strict decoding, and `-`
// meaning stdin.
//
// Why this exists as a CLI: pkg/client/client.go's Coverage census named
// /api/v1/drives as SDK-uncovered since 0.7 — an admin who wanted a drive
// scripted or backed up had no way to do it besides the console or raw HTTP,
// even though the family already has exactly the shape `get`/`set` needs.
//
//	wardyn drive get > drives.json      # a snapshot, or before a reset
//	wardyn drive set drives.json      # restore, or hand-authored additions
//
// `set` UPSERTS every drive and grant the file names, over the existing
// POST /drives, PUT /drives/{id} and POST /drives/grants routes — there is no
// bulk-write route, and this command adds none. A drive with an id `get`
// already issued is REPLACED in place; one with none is CREATED. Nothing the
// file omits is touched, and nothing is deleted — `get` immediately followed
// by `set` is therefore a no-op, the same round trip site-config's pair
// promises for the operator baseline.
func driveCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive",
		Short: "Get or upsert the admin-registered drives and their allocations; reclaim one person's storage",
		Long: "Read or upsert the admin-registered drives (storage an admin allocates to a\n" +
			"user, a group, or everyone) and their allocations:\n\n" +
			"    wardyn drive get > drives.json\n" +
			"    wardyn drive set drives.json\n\n" +
			"`set` upserts every drive and grant the file names — a drive whose id `get`\n" +
			"already issued is REPLACED in place, one with none is CREATED, and a grant is\n" +
			"upserted by its (subject_type, subject, drive_id) key, beside the subject's\n" +
			"other drives. Nothing the file\n" +
			"omits is touched and nothing is deleted, so `wardyn drive get > f && wardyn\n" +
			"drive set f` is a no-op.\n\n" +
			"`reclaim` is the one verb that destroys data: see `wardyn drive reclaim --help`.\n" +
			"`member` lists and deletes the drives people create for themselves.",
	}
	cmd.AddCommand(driveGetCmd(client), driveSetCmd(client), driveReclaimCmd(client), driveMemberCmd(client))
	return subcommandGroup(cmd)
}

func driveGetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "get",
		Short: "Print every registered drive and allocation as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := client().GetDrives(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), doc)
		},
	}
}

func driveSetCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "set [file]",
		Short: "Upsert the drives and allocations in a JSON file (or stdin with '-')",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := "-"
			if len(args) == 1 {
				src = args[0]
			}
			var raw []byte
			var err error
			if src == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(src)
			}
			if err != nil {
				return fmt.Errorf("read drives document: %w", err)
			}
			// Strict decode, the same shape as site-config's own set and the
			// server's decodeStrict: a key this file mistypes must surface as a
			// parse error, not silently vanish from what ApplyDrives then sends.
			var doc sdk.DrivesDocument
			if err := decodeOneJSONStrict(bytes.NewReader(raw), &doc); err != nil {
				return fmt.Errorf("parse drives document JSON: %w", err)
			}
			res, err := client().ApplyDrivesResult(cmd.Context(), doc)
			if err != nil {
				if len(res.Steps) > 0 {
					printApplySteps(cmd.ErrOrStderr(), res.Steps)
				}
				return err
			}
			return emitJSON(cmd.OutOrStdout(), res.Document)
		},
	}
}

// `wardyn drive reclaim` — the offboarding half of user drives that is NOT a
// console screen (#166).
//
// It destroys data: `reclaim` deletes the storage object one person's
// allocation resolved to, on whichever substrate this deployment dispatches.
// There is no console button for it in 0.8 — a destructive confirmation is a
// screen, and this one has no approved mock — so the API and this command are
// the whole surface.
//
// RAW HTTP RATHER THAN AN SDK METHOD, deliberately. pkg/client's drives family
// wraps the get/set pair only (GetDrives, ApplyDrives — see client.go's
// Coverage block); this destructive verb is not added to it.
// `mintAttachTicket` in attach.go reaches its deliberately-unwrapped route the
// same way.
//
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
	var subject, subjectType, homeSubject string
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

--subject is the key the allocation is stored under. When that is an email or
another alias, also pass --home-subject with the person's sign-in subject: a run
derives the storage name from the sign-in subject, so the alias alone names a
different object, and an allocation keyed by an alias is refused without it.

Super-admin only. Refused with a 409 while a run still holds the object, or
while the object answering to that name is not this drive's. Every attempt —
successes, refusals and failures alike — is audited as ` + "`drive.reclaim`" + `.

On Kubernetes the daemon holds no delete verb on claims unless the chart's
drives.reclaim.enabled is set, so on a stock install every attempt fails
with the apiserver's own 403. See docs/OPERATIONS.md.

The outcome is 'deleted' (this call destroyed the storage) or 'already_absent'
(nothing answered to the name, so nothing was destroyed).
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDriveReclaim(cmd, client(), args[0], subjectType, subject, homeSubject, yes, asJSON)
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "the subject the allocation is keyed on: the person's sign-in subject, or their email (required)")
	cmd.Flags().StringVar(&homeSubject, "home-subject", "", "the person's sign-in subject, when --subject is another of their claims (an email)")
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
func runDriveReclaim(cmd *cobra.Command, c *sdk.Client, id, subjectType, subject, homeSubject string, yes, asJSON bool) error {
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
	req := map[string]string{"subject_type": subjectType, "subject": subject}
	if strings.TrimSpace(homeSubject) != "" {
		req["home_subject"] = homeSubject
	}
	body, err := json.Marshal(req)
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
	resp, err := rawHTTPClient(c).Do(req)
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

// driveMemberCmd is `wardyn drive member`: the drives people create for
// themselves, which `drive get` does not list. Super admin only.
func driveMemberCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "List or delete the drives people create for themselves (super admin)",
	}
	cmd.AddCommand(driveMemberListCmd(client), driveMemberDeleteCmd(client))
	return subcommandGroup(cmd)
}

func driveMemberListCmd(client clientFn) *cobra.Command {
	var limit, offset int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List people's drives with their owner and storage object",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := client().ListMemberDrives(cmd.Context(), sdk.ListOpts{Limit: limit, Offset: offset})
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), list)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tNAME\tOWNER\tBACKEND\tOBJECT\tSTATE")
			for _, d := range list.Drives {
				state := "ready"
				if d.Deleting {
					state = "deleting"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.Owner, d.Backend, d.Object, state)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if shown := offset + len(list.Drives); shown < list.Total {
				fmt.Fprintf(cmd.ErrOrStderr(), "%d of %d drives shown; pass --offset %d for more\n", shown, list.Total, shown)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "most drives to list (0 = the server's default)")
	cmd.Flags().IntVar(&offset, "offset", 0, "drives to skip")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {drives, total} as JSON")
	return cmd
}

func driveMemberDeleteCmd(client clientFn) *cobra.Command {
	var yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "delete <drive-id>",
		Short: "DELETE one person's drive and its files",
		Long: `Delete a drive a person created, and the Docker volume or claim holding its
files. THIS DELETES DATA AND NOTHING UNDOES IT.

Refused with a 409 while a run still holds the drive. Every attempt is audited
as ` + "`drive.member.delete`" + `. On Kubernetes the daemon needs the chart's
drives.reclaim.enabled to delete claims.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(strings.TrimSpace(args[0]))
			if err != nil {
				return fmt.Errorf("drive member delete: %q is not a drive id (a uuid)", args[0])
			}
			if !yes {
				return fmt.Errorf("drive member delete: refusing without --yes: this permanently deletes drive %s and its files", id)
			}
			res, err := client().DeleteMemberDrive(cmd.Context(), id)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s) of drive %q owned by %s\n", res.Outcome, res.Object, res.Backend, res.Drive, res.Owner)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm that this permanently deletes the person's drive and its files")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {drive, drive_id, owner, backend, object, outcome} as JSON")
	return cmd
}
