// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// maxImpactListed is how many people and live runs `changes show --impact` lists; the rest are counted.
const maxImpactListed = 10

// noMinimumSentence is what `approvals-needed get` prints when the deploy-time minimum is not set. It says
// nothing about changes already pending: each keeps the requirement it was proposed with.
const noMinimumSentence = "Deploy-time minimum not set. New changes need one approval. Pending changes keep their existing approval requirements."

// changeShowCmd prints one change: what it is, what the server read of it, who has approved it and who
// still can, and with --impact whom approving it would reach.
func changeShowCmd(client clientFn) *cobra.Command {
	var impact, asJSON bool
	cmd := &cobra.Command{
		Use:   "show <change-id>",
		Short: "Show a governance change: what it does, who approved it and who can still approve",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("governance change", args[0])
			if err != nil {
				return err
			}
			c := client()
			ch, err := c.GetGovernanceChange(cmd.Context(), id)
			if err != nil {
				return err
			}
			var im *sdk.GovernanceChangeImpact
			if impact {
				got, err := c.GetGovernanceChangeImpact(cmd.Context(), id)
				if err != nil {
					return err
				}
				im = &got
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), struct {
					sdk.GovernanceChange
					Impact *sdk.GovernanceChangeImpact `json:"impact,omitempty"`
				}{ch, im})
			}
			w := cmd.OutOrStdout()
			printChange(w, ch)
			if im != nil {
				printImpact(w, *im)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&impact, "impact", false, "also show who and what approving the change would reach (a pending change only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// changeTakeBackCmd withdraws the caller's own approval of a pending change.
func changeTakeBackCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "take-back <change-id>",
		Short: "Take back your approval of a pending governance change",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("governance change", args[0])
			if err != nil {
				return err
			}
			ch, err := client().TakeBackGovernanceApproval(cmd.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Took back your approval of governance change %s", ch.ID)
			if a := ch.Approvals; a != nil {
				fmt.Fprintf(cmd.OutOrStdout(), ": %d of %d approved", a.Have, a.Required)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
}

// approveChange approves a pending change, with the approver's reason when there is one.
func approveChange(cmd *cobra.Command, c *sdk.Client, id string, reason string) error {
	uid, err := parseID("governance change", id)
	if err != nil {
		return err
	}
	if err := checkReasonLength(reason); err != nil {
		return err
	}
	approve := c.ApproveGovernanceChange
	if reason != "" {
		approve = func(ctx context.Context, id uuid.UUID) (sdk.GovernanceChange, error) {
			return c.ApproveGovernanceChangeWithReason(ctx, id, reason)
		}
	}
	ch, err := approve(cmd.Context(), uid)
	if err != nil {
		return err
	}
	if a := ch.Approvals; a != nil && ch.State == "pending" {
		fmt.Fprintf(cmd.OutOrStdout(), "Recorded: %d of %d approved\n", a.Have, a.Required)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "governance change %s -> %s\n", ch.ID, ch.State)
	}
	if ch.TokensRevocationFailed {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", ch.Detail)
	}
	return nil
}

// printChange writes what a change is, what the server read of it, and where its approvals stand.
func printChange(w io.Writer, ch sdk.GovernanceChange) {
	fmt.Fprintf(w, "Change:   %s\n", ch.ID)
	fmt.Fprintf(w, "Kind:     %s\n", ch.TargetKind)
	fmt.Fprintf(w, "Op:       %s\n", ch.Op)
	fmt.Fprintf(w, "Target:   %s\n", ch.TargetKey)
	fmt.Fprintf(w, "State:    %s\n", ch.State)
	fmt.Fprintf(w, "%s\n", reviewVerdictLine(ch.Diff.Review))
	if r := ch.Diff.Review; r != nil && r.Risk != nil {
		fmt.Fprintf(w, "Risk:     %s\n", riskLine(*r.Risk))
	}
	printApprovals(w, ch)
}

// reviewVerdictLine is the one-line verdict of a change's review; no review reads as not checked.
func reviewVerdictLine(r *sdk.GovernanceChangeReview) string {
	switch {
	case r == nil:
		return "Not checked"
	case r.Verdict == "widens":
		parts := make([]string, 0, len(r.Widens))
		for _, wd := range r.Widens {
			part := wd.Field
			if n := len(wd.Entries); n > 0 {
				part += fmt.Sprintf(" (+%d: %s)", n, strings.Join(wd.Entries, ", "))
			}
			parts = append(parts, part)
		}
		line := "Widens: " + strings.Join(parts, "; ")
		if len(r.HeldBecause) > 0 {
			line += "; also held because: " + strings.Join(r.HeldBecause, ", ")
		}
		return line
	case r.Verdict == "narrows_held":
		return "Narrows, but held because: " + strings.Join(r.HeldBecause, ", ")
	}
	return "Not checked"
}

// riskLine is a change's risk grade as "low → medium"; a create has no grade before it.
func riskLine(r sdk.GovernanceChangeRisk) string {
	if r.Before == nil {
		return r.After.Overall + " (new)"
	}
	return r.Before.Overall + " → " + r.After.Overall
}

// tierWords is an approver's tier in plain words.
func tierWords(tier, unit string) string {
	words := strings.ReplaceAll(tier, "_", " ")
	if unit != "" {
		words += " of " + unit
	}
	return words
}

// droppedWords is why an approval no longer counts, in plain words.
func droppedWords(why string) string {
	switch why {
	case "deactivated":
		return "they are deactivated"
	case "token_revoked":
		return "the token they approved with was revoked"
	case "session_revoked":
		return "their sessions were revoked"
	case "role_removed":
		return "they no longer hold the role"
	case "grant_removed":
		return "their unit admin grant was removed"
	case "cant_confirm":
		return "their role cannot be confirmed"
	}
	return why
}

// printApprovals writes where a change's approvals stand; a pending change without them is decided by one person.
func printApprovals(w io.Writer, ch sdk.GovernanceChange) {
	a := ch.Approvals
	if a == nil {
		// The server sends the block for a pending change only, so a decided change says nothing here.
		if ch.State == "pending" {
			fmt.Fprintln(w, "One person decides.")
		}
		return
	}
	head := fmt.Sprintf("Approvals: %d of %d approved", a.Have, a.Required)
	if a.RequireSecurity {
		head += " (one must be a security admin)"
	}
	fmt.Fprintln(w, head)
	for _, e := range a.Entries {
		who := fmt.Sprintf("  %s (%s)", e.Approver, tierWords(e.Tier, e.UnitName))
		if e.Dropped != "" || !e.Counts {
			fmt.Fprintf(w, "%s no longer counts: %s\n", who, droppedWords(e.Dropped))
			continue
		}
		line := who + " approved " + e.ApprovedAt.UTC().Format(time.RFC3339)
		if e.Reason != "" {
			line += ": " + e.Reason
		}
		fmt.Fprintln(w, line)
	}
	people := make([]string, 0, len(a.Eligible))
	for _, p := range a.Eligible {
		people = append(people, firstNonEmpty(p.Email, p.Principal))
	}
	fmt.Fprintf(w, "Can approve: %s\n", firstNonEmpty(strings.Join(people, ", "), "no one"))
	if a.ProposerActive != nil && !*a.ProposerActive {
		fmt.Fprintln(w, "Proposer: no longer active, so the last approval will be refused")
		if a.Reach == "ok" {
			return
		}
	}
	fmt.Fprintln(w, reachLine(a))
}

// firstNonEmpty is a, or b when a is empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reachLine says whether enough people can still approve. When none who can is a security admin it says so
// and gives no number; when there are too few it gives the numbers.
func reachLine(a *sdk.GovernanceChangeApprovals) string {
	switch a.Reach {
	case "ok":
		return "Reach:    enough people can still approve"
	case "cant_confirm":
		return "Reach:    cannot confirm that enough people can still approve"
	case "cant_reach":
		if a.CantReachReason == "security" {
			return "Reach:    no one who can still approve is a security admin"
		}
		return fmt.Sprintf("Reach:    not enough people can still approve (approvals still needed: %d, people who can still give one: %d)", valueOr0(a.StillNeeded), valueOr0(a.RemainingEligible))
	}
	return "Reach:    not reported"
}

// valueOr0 is what n points at, or 0 when the server sent nothing.
func valueOr0(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// printImpact writes what approving a change would reach.
func printImpact(w io.Writer, im sdk.GovernanceChangeImpact) {
	switch im.Computed {
	case "tree":
		fmt.Fprintf(w, "Impact (as of %s): %d people, %d live runs, %d units may become a context, %d may stop\n",
			im.AsOf.UTC().Format(time.RFC3339), len(im.People), valueOr0(im.LiveRunsTotal), len(im.Activates), len(im.Vacates))
		printImpactPeople(w, im.People)
		printImpactRuns(w, im.LiveRuns, valueOr0(im.LiveRunsTotal))
	case "boundary":
		b := im.Boundary
		if b == nil {
			b = &sdk.GovernanceImpactBoundary{}
		}
		fmt.Fprintf(w, "Impact (as of %s): the boundary narrows %d deployment knobs, %d units, %d stored policies, %d assignments and %d provider rows\n",
			im.AsOf.UTC().Format(time.RFC3339), len(b.Deployment), len(b.Units), len(b.StoredPolicies), len(b.Memberships), len(b.ProviderRows))
	default:
		fmt.Fprintf(w, "Impact: not computed: %s\n", notComputedWords(im.NotComputed))
	}
}

// notComputedWords is why an impact was not computed.
func notComputedWords(why string) string {
	switch why {
	case "kind":
		return "this kind of change has no preview"
	case "stale":
		return "the target changed since the change was proposed"
	case "refused":
		return "the change would be refused if it were applied now"
	}
	return why
}

// printImpactPeople lists the first people an impact reaches.
func printImpactPeople(w io.Writer, people []sdk.GovernanceImpactPerson) {
	for i, p := range people {
		if i == maxImpactListed {
			fmt.Fprintf(w, "  and %d more people\n", len(people)-i)
			return
		}
		line := fmt.Sprintf("  %s %s: %s -> %s", p.SubjectType, p.Subject, p.FromUnitName, p.UnitName)
		if p.Error != "" {
			line += " (does not compose: " + p.Error + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// printImpactRuns lists the first live runs an impact reaches, with their owners. total counts them all.
func printImpactRuns(w io.Writer, runs []sdk.GovernanceImpactRun, total int) {
	for i, r := range runs {
		if i == maxImpactListed {
			break
		}
		fmt.Fprintf(w, "  run %s owned by %s\n", r.ID, r.Owner)
	}
	if more := total - min(len(runs), maxImpactListed); more > 0 {
		fmt.Fprintf(w, "  and %d more live runs\n", more)
	}
}

// governanceApprovalsNeededCmd is `governance approvals-needed`: how many approvals each kind of change needs.
func governanceApprovalsNeededCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals-needed",
		Short: "Show or set how many approvals each kind of governance change needs",
	}
	var asJSON bool
	get := &cobra.Command{
		Use:   "get",
		Short: "Show how many approvals each kind of change needs, and who can give them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			counts, err := client().GetApprovalCounts(cmd.Context())
			if err != nil {
				return noApprovalCounts(err)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), counts)
			}
			return printApprovalCounts(cmd.OutOrStdout(), counts)
		},
	}
	get.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")

	var file string
	set := &cobra.Command{
		Use:   "set",
		Short: "Replace the approval counts from a JSON file; the edit is held for approval unless made with the admin token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setApprovalCounts(cmd, client(), file)
		},
	}
	set.Flags().StringVar(&file, "file", "", "the JSON body: {\"default\": {\"required\", \"require_security\"}, \"kinds\": [{\"kind\", \"required\", \"require_security\"}]} (- for stdin)")
	_ = set.MarkFlagRequired("file")
	cmd.AddCommand(get, set)
	return subcommandGroup(cmd)
}

// setApprovalCounts sends the counts in file. An edit the server holds for a second approver is reported
// as pending and exits 0, as every held governance write does.
func setApprovalCounts(cmd *cobra.Command, c *sdk.Client, file string) error {
	var raw io.Reader = cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("read approval counts: %w", err)
		}
		defer f.Close()
		raw = f
	}
	var body sdk.ApprovalCountsWrite
	if err := decodeOneJSONStrict(raw, &body); err != nil {
		return fmt.Errorf("read approval counts: %w", err)
	}
	counts, err := c.PutApprovalCounts(cmd.Context(), body)
	var pending *sdk.PendingApprovalError
	if errors.As(err, &pending) {
		printGovernancePending(cmd.ErrOrStderr(), sdk.GovernanceApplyResult{Pending: pending.Changes})
		return nil
	}
	if err != nil {
		return noApprovalCounts(err)
	}
	return printApprovalCounts(cmd.OutOrStdout(), counts)
}

// noApprovalCounts words the 404 of a server that has no approval counts route; any other error is unchanged.
func noApprovalCounts(err error) error {
	var api *sdk.APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return errors.New("This server does not offer approval counts yet.")
	}
	return err
}

// approverWords is who may approve a kind, in plain words.
func approverWords(approvers string) string {
	switch approvers {
	case "super_admin":
		return "super admins"
	case "security":
		return "security admins"
	case "security_or_unit_admin":
		return "security admins or unit admins"
	}
	return approvers
}

// poolWords is how many people can approve a kind now.
func poolWords(p sdk.ApprovalCountsPool) string {
	n := fmt.Sprintf("%d", p.Count)
	switch p.Status {
	case "exact":
		n += " (exactly enough)"
	case "short":
		n += " (too few)"
	case "cant_confirm":
		n = "cannot confirm"
		if p.Why == "too_many_people" {
			n += " (too many people to count)"
		} else {
			n += " (some people's roles are unconfirmed)"
		}
	}
	if p.UnitName != "" {
		n += " in " + p.UnitName
	}
	return n
}

// printApprovalCounts writes the approval counts as a table, or says the deploy-time minimum is not set.
func printApprovalCounts(w io.Writer, c sdk.ApprovalCounts) error {
	if c.Minimum == 0 {
		_, err := fmt.Fprintln(w, noMinimumSentence)
		return err
	}
	tw := newTab(w)
	fmt.Fprintln(tw, "KIND\tAPPROVALS\tSECURITY ADMIN\tWHO CAN APPROVE\tCAN APPROVE NOW")
	fmt.Fprintf(tw, "(default)\t%d\t%s\t\t\n", c.Default.Required, yesNo(c.Default.RequireSecurity))
	for _, k := range c.Kinds {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", k.Kind, k.Required, yesNo(k.RequireSecurity), approverWords(k.Approvers), poolWords(k.Pool))
	}
	return tw.Flush()
}
