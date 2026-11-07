// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// Exit codes of `wardyn audit verify`. Everything else follows the CLI's own contract.
const (
	verifyExitFailed   = 1 // a check failed
	verifyExitUsage    = 2 // the bundle or the keys could not be read
	verifyExitWithheld = 3 // nothing failed, but an event's content was not checked at this tier
)

// receiptWaitDefault is how often --wait asks again when the server sends no Retry-After.
const receiptWaitDefault = 5 * time.Second

// writeAudit writes data to path whole or not at all, or to w when path is empty.
func writeAudit(w io.Writer, path string, data []byte) error {
	if path == "" {
		_, err := w.Write(data)
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wardyn-audit-*.part")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return finalizePartFile(f.Name(), path, werr)
}

func marshalAudit(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// auditReceiptCmd fetches a signed receipt: proof, checkable offline with `audit verify`, that audit rows are in
// the log unaltered.
func auditReceiptCmd(client clientFn) *cobra.Command {
	var runID, outPath string
	var afterSeq int64
	var limit int
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "receipt <event-id>... | --run <run-id>",
		Short: "Fetch a signed receipt that audit events are in the log unaltered (security admin; stdout unless -o)",
		Long: "Fetch a receipt for the named audit events, or for the events of one run, and save it with -o.\n" +
			"An event is receiptable once the checkpoint covering it is signed, about a minute after it is\n" +
			"written; --wait asks again until then. A run's receipt is a page of at most 1000 events: when it\n" +
			"ends before the run does, the next --after-seq is named on stderr. Check the file with\n" +
			"`wardyn audit verify <file> --keys <keys.json>`.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := sdk.AuditReceiptQuery{AfterSeq: afterSeq, Limit: limit}
			for _, a := range args {
				id, err := uuid.Parse(a)
				if err != nil {
					return usageError(fmt.Errorf("event id %q is not a UUID", a))
				}
				q.EventIDs = append(q.EventIDs, id)
			}
			if runID != "" {
				id, err := uuid.Parse(runID)
				if err != nil {
					return usageError(fmt.Errorf("--run %q is not a UUID", runID))
				}
				q.RunID = id
			}
			switch {
			case (runID != "") == (len(args) > 0):
				return usageError(errors.New("name audit event ids, or one run with --run, not both and not neither"))
			case runID == "" && (afterSeq != 0 || limit != 0):
				return usageError(errors.New("--after-seq and --limit page a run's receipt: use them with --run"))
			}
			ctx := cmd.Context()
			if wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			c := client()
			var b sdk.AuditBundle
			for {
				var h http.Header
				var err error
				b, h, err = c.AuditReceipt(ctx, q)
				if err == nil {
					break
				}
				var apiErr *sdk.APIError
				if !errors.As(err, &apiErr) || apiErr.Reason != "audit_receipt_pending" {
					return err
				}
				if !wait {
					return fmt.Errorf("%w (the checkpoint covering it is not signed yet: retry with --wait)", err)
				}
				d := receiptWaitDefault
				if s, convErr := strconv.Atoi(h.Get("Retry-After")); convErr == nil && s >= 0 {
					d = time.Duration(s) * time.Second
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "waiting %s for the next checkpoint...\n", d)
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return fmt.Errorf("timed out waiting for the checkpoint that covers the events: %w", apiErr)
				}
			}
			raw, err := marshalAudit(b.Bundle)
			if err != nil {
				return err
			}
			if err := writeAudit(cmd.OutOrStdout(), outPath, raw); err != nil {
				return fmt.Errorf("audit receipt did not complete: %w", err)
			}
			events := 0
			for _, cp := range b.Checkpoints {
				events += len(cp.Events)
			}
			if outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d event(s) in %d checkpoint(s)\n", outPath, events, len(b.Checkpoints))
			}
			if b.NextAfterSeq > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "this receipt ends at seq %d and the run has more events: run again with --after-seq %d for the next page\n", b.NextAfterSeq, b.NextAfterSeq)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "receipt for every audit event of this run")
	cmd.Flags().Int64Var(&afterSeq, "after-seq", 0, "with --run: continue after this seq (the previous page named it)")
	cmd.Flags().IntVar(&limit, "limit", 0, "with --run: at most this many events (the server's cap, 1000, when 0)")
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "write the receipt here instead of stdout")
	cmd.Flags().BoolVar(&wait, "wait", false, "when an event is newer than the last signed checkpoint, ask again until it is covered")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "with --wait: give up after this long")
	return cmd
}

// auditCheckpointsCmd downloads signed checkpoints: signatures and links only, no row and no inclusion proof.
func auditCheckpointsCmd(client clientFn) *cobra.Command {
	var outPath string
	var afterSeq int64
	var limit int
	cmd := &cobra.Command{
		Use:   "checkpoints",
		Short: "Download signed audit checkpoints, to pin or compare heads (security admin; stdout unless -o)",
		Long: "Download the signed audit checkpoints covering rows above --after-seq, oldest first. The file proves\n" +
			"each checkpoint was signed and that none was removed; it proves no row is in the log (that is a\n" +
			"receipt). `wardyn audit verify` says so: \"signature checked, inclusion not checked\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := client().AuditCheckpoints(cmd.Context(), afterSeq, limit)
			if err != nil {
				return err
			}
			raw, err := marshalAudit(b.Bundle)
			if err != nil {
				return err
			}
			if err := writeAudit(cmd.OutOrStdout(), outPath, raw); err != nil {
				return fmt.Errorf("audit checkpoints did not complete: %w", err)
			}
			if outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d checkpoint(s)\n", outPath, len(b.Checkpoints))
			}
			if b.NextAfterSeq > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "more checkpoints follow: run again with --after-seq %d\n", b.NextAfterSeq)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "write the export here instead of stdout")
	cmd.Flags().Int64Var(&afterSeq, "after-seq", 0, "only checkpoints covering rows above this seq")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many checkpoints (the server's cap, 1000, when 0)")
	return cmd
}

// auditKeysCmd downloads the public keys checkpoints are signed with.
func auditKeysCmd(client clientFn) *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Download the public keys audit checkpoints are signed with (security admin; stdout unless -o)",
		Long: "Download the public keys, current and previous. Save the file now, somewhere the control plane cannot\n" +
			"change: `wardyn audit verify --keys` trusts only what this file lists, and a file fetched after an\n" +
			"incident is only as good as the control plane that served it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			keys, err := client().AuditReceiptKeys(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := marshalAudit(keys)
			if err != nil {
				return err
			}
			if err := writeAudit(cmd.OutOrStdout(), outPath, raw); err != nil {
				return fmt.Errorf("audit keys did not complete: %w", err)
			}
			if outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d key(s)\n", outPath, len(keys.Keys))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "write the keys here instead of stdout")
	return cmd
}

// auditVerifyCmd checks a receipt or a checkpoint export against pinned public keys. It takes no client, makes
// no request and reads no configuration: the bundle and the keys file are the whole input, so the verdict does
// not depend on the control plane being honest or reachable.
func auditVerifyCmd() *cobra.Command {
	var keysPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify <bundle>",
		Short: "Check a receipt or checkpoint export offline against pinned keys",
		Long: "Check a file from `wardyn audit receipt` or `wardyn audit checkpoints` with the public keys in --keys,\n" +
			"from `wardyn audit keys`. Nothing is fetched. Exit 0: every receipted row is what was signed. Exit 1:\n" +
			"a check failed (the line names the seq and the reason). Exit 2: the bundle or keys could not be read.\n" +
			"Exit 3: nothing failed, but some events were served without host detail for your role, so they are\n" +
			"linked into the signed list and their content was not checked: the first line says how many.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError(errors.New("verify takes one bundle file"))
			}
			if keysPath == "" {
				return usageError(errors.New("--keys is required: the public keys pinned from `wardyn audit keys`; a bundle never vouches for its own key"))
			}
			var keys auditreceipt.Keys
			if err := readJSONFile(keysPath, &keys); err != nil {
				return usageError(fmt.Errorf("keys file: %w", err))
			}
			if keys.Format != auditreceipt.KeysFormat {
				return usageError(fmt.Errorf("keys file: format %q is not %q", keys.Format, auditreceipt.KeysFormat))
			}
			var b auditreceipt.Bundle
			if err := readJSONFile(args[0], &b); err != nil {
				return usageError(fmt.Errorf("bundle: %w", err))
			}
			rep := auditreceipt.VerifyBundle(b, keys)
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
			} else {
				printVerifyReport(cmd.OutOrStdout(), rep)
			}
			switch {
			case !rep.OK():
				return &exitError{code: verifyExitFailed, err: fmt.Errorf("audit verify: %d check(s) failed", len(rep.Failures))}
			case !rep.Complete():
				return &exitError{code: verifyExitWithheld, err: fmt.Errorf("audit verify: %d event(s) were not checked at your role", rep.Withheld())}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&keysPath, "keys", "", "public keys file from `wardyn audit keys`, saved where the control plane cannot change it")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full report as JSON")
	return cmd
}

func readJSONFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return nil
}

// printVerifyReport writes the verdict: the withheld count first when there is one, then each checkpoint, event
// and policy line, every failure with its seq and reason, and the bottom line.
func printVerifyReport(w io.Writer, rep auditreceipt.Report) {
	if n := rep.Withheld(); n > 0 {
		fmt.Fprintf(w, "%d event(s) withheld: linked into the signed list, content not re-hashed at your role\n", n)
	}
	fmt.Fprintf(w, "%s: %d checkpoint(s), %d event(s)\n", rep.Format, len(rep.Checkpoints), len(rep.Events))
	for i, c := range rep.Checkpoints {
		state := "signature checked, inclusion not checked"
		switch {
		case !c.Signed:
			state = "signature NOT verified"
		case c.Inclusion:
			state = "signature and hash list checked"
		}
		fmt.Fprintf(w, "checkpoint %d seq %d-%d kid %.8s: %s\n", i, c.SeqLo, c.SeqHi, c.Kid, state)
	}
	for _, e := range rep.Events {
		state := "FAILED"
		switch {
		case e.Withheld:
			state = "WITHHELD (linked, content not re-hashed at your role)"
		case e.OK:
			state = "verified"
		}
		fmt.Fprintf(w, "event seq %d %s %s: %s\n", e.Seq, e.ID, e.Action, state)
	}
	for _, p := range rep.Policies {
		switch p.Status {
		case auditreceipt.PolicyLinked:
			fmt.Fprintf(w, "policy seq %d %s: matching policy content at seq %d\n", p.Seq, p.Action, p.Resolve)
		case auditreceipt.PolicyUnlinked:
			fmt.Fprintf(w, "policy seq %d %s: stamped %.12s, no matching policy content in this bundle\n", p.Seq, p.Action, p.Digest)
		default:
			fmt.Fprintf(w, "policy seq %d %s: %s\n", p.Seq, p.Action, p.Status)
		}
	}
	for _, f := range rep.Failures {
		where := "bundle"
		if f.Checkpoint >= 0 {
			where = fmt.Sprintf("checkpoint %d", f.Checkpoint)
		}
		if f.Seq > 0 {
			where += fmt.Sprintf(" seq %d", f.Seq)
		}
		fmt.Fprintf(w, "FAIL %s %s: %s\n", where, f.Reason, f.Detail)
	}
	switch {
	case !rep.OK():
		fmt.Fprintf(w, "FAILED: %d check(s) failed\n", len(rep.Failures))
	case !rep.Complete():
		fmt.Fprintf(w, "INCOMPLETE: nothing failed, but %d event(s) were not checked\n", rep.Withheld())
	default:
		fmt.Fprintln(w, "OK")
	}
}
