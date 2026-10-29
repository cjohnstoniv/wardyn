// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// apply.go — `wardyn apply|diff|delete -f FILE`: keep the workloads a file names.
//
//	wardyn apply -f web.yaml          # create or update; a run starts when the spec is new or changed
//	wardyn diff -f web.yaml           # exit 0 when the stored workload equals the file, 1 when not
//	wardyn delete -f web.yaml         # end each workload's run and delete it
//
// A file holds one or more YAML or JSON documents, each a `wardyn/v1` Workload or a
// `wardyn.dev/v1alpha1` WardynWorkload (read under its spec; the namespace is the cluster's
// business, not an apply's). The workload is the caller's own: nothing here names another owner.

// fileFlag is the -f/--file flag all three verbs share.
func fileFlag(cmd *cobra.Command, file *string) {
	cmd.Flags().StringVarP(file, "file", "f", "", "the workload file: YAML or JSON, one or more documents ('-' for stdin)")
	_ = cmd.MarkFlagRequired("file")
}

// readWorkloadDocs reads every document of file ('-' for stdin) as a workload. YAML is a JSON
// superset, so one reader takes both; each document is converted to JSON for sdk.ParseWorkload,
// which refuses a field it does not know. Empty documents between `---` marks are skipped, and
// two documents of one name are refused: the second would silently win.
func readWorkloadDocs(cmd *cobra.Command, file string) ([]sdk.WorkloadDocument, error) {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, fmt.Errorf("read the workload file: %w", err)
	}
	var docs []sdk.WorkloadDocument
	seen := map[string]bool{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for n := 1; ; n++ {
		var v any
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("document %d: parse (accepts JSON or YAML): %w", n, err)
		}
		if v == nil {
			continue
		}
		js, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("document %d: re-encode as JSON: %w", n, err)
		}
		doc, err := sdk.ParseWorkload(js)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", n, err)
		}
		if seen[doc.Name] {
			return nil, fmt.Errorf("document %d: workload %q is named twice in the file", n, doc.Name)
		}
		seen[doc.Name] = true
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, errors.New("the workload file holds no document")
	}
	return docs, nil
}

func workloadApplyCmd(client clientFn) *cobra.Command {
	var file string
	var wait, dryRun, asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Create or update the workloads in a file, and start a run when one is new or changed",
		Long: "Store each workload the file names and keep it running. An unchanged workload does nothing; a new or\n" +
			"changed one starts a new run and then ends the old one. If the organisation refuses the new run, the\n" +
			"spec is still stored, the old run keeps running, and the command exits 1.\n\n" +
			"    wardyn apply -f web.yaml\n" +
			"    wardyn apply -f - --wait < web.yaml\n\n" +
			"--wait follows each launched run until it is running, or until it completes when the workload is an\n" +
			"agent task. --dry-run writes nothing: it prints what would change and the setup checklist of the run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun && wait {
				return errors.New("--dry-run and --wait are mutually exclusive (a dry run starts nothing)")
			}
			docs, err := readWorkloadDocs(cmd, file)
			if err != nil {
				return err
			}
			if dryRun {
				return workloadDryRun(cmd, client(), docs, asJSON)
			}
			c := client()
			results := make([]sdk.WorkloadApplyResult, 0, len(docs))
			refused := 0
			for _, d := range docs {
				res, err := c.ApplyWorkload(cmd.Context(), d.Name, d.Request())
				if err != nil {
					return fmt.Errorf("workload/%s: %w", d.Name, err)
				}
				results = append(results, res)
				if res.Launch.State == sdk.WorkloadRefused {
					refused++
				}
				printWorkloadResult(cmd, d.Name, string(res.Outcome), res, asJSON)
			}
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), results); err != nil {
					return err
				}
			}
			if wait {
				for i, res := range results {
					if res.Launch.State != sdk.WorkloadLaunched || res.Launch.RunID == nil {
						continue
					}
					if err := followWorkloadRun(cmd, c, docs[i], *res.Launch.RunID, timeout); err != nil {
						return err
					}
				}
			}
			if refused > 0 {
				return &exitError{code: 1, err: fmt.Errorf("%d workload(s) refused: the spec is stored and the previous run keeps running", refused)}
			}
			return nil
		},
	}
	fileFlag(cmd, &file)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "write nothing: print the diff against the stored workload and the setup checklist of the run it would start")
	cmd.Flags().BoolVar(&wait, "wait", false, "follow each launched run until it is running (until it completes, for an agent task); exit with the run's outcome")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up waiting after this long (with --wait; exit 124). The run keeps going")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the results as a JSON array (progress goes to stderr)")
	return cmd
}

// printWorkloadResult says what an apply or restart did: one line on stdout (none with --json,
// which prints the results itself), and the server's warnings on stderr. verb is the outcome,
// or "restarted". A refused launch names its reason and sentence.
func printWorkloadResult(cmd *cobra.Command, name, verb string, res sdk.WorkloadApplyResult, asJSON bool) {
	if !asJSON {
		switch l := res.Launch; {
		case l.State == sdk.WorkloadRefused:
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s refused %s: %s\n", name, l.Reason, l.Message)
		case l.RunID != nil:
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s %s run %s\n", name, verb, l.RunID)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s %s\n", name, verb)
		}
	}
	for _, w := range res.Launch.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "  warning: %s\n", w)
	}
}

// followWorkloadRun waits for the run an apply launched: to COMPLETED for an agent task, which
// ends by itself, and to RUNNING for anything else, which is kept up. Both exit as `run --wait`.
func followWorkloadRun(cmd *cobra.Command, c *sdk.Client, d sdk.WorkloadDocument, id uuid.UUID, timeout time.Duration) error {
	req, err := sdk.RunDocumentRequest(d.Intent)
	if err != nil {
		return err
	}
	task := req.Workload != nil && req.Workload.Kind == sdk.WorkloadAgentTask ||
		req.Workload == nil && req.Task != "" && req.Experience != sdk.ExperienceInteractive
	if task {
		return waitForRun(cmd.Context(), cmd.ErrOrStderr(), c, id, timeout)
	}
	return waitForRunRunning(cmd.Context(), cmd.ErrOrStderr(), c, id, timeout)
}

// waitForRunRunning polls until the run is RUNNING. A run that ends first exits as waitForRun
// would (FAILED→1, KILLED/STOPPED/ARCHIVED→2); one that completes before a poll saw it running
// is a success. The timeout bounds every request, as in waitForRun (124).
func waitForRunRunning(ctx context.Context, errW io.Writer, c *sdk.Client, runID uuid.UUID, timeout time.Duration) error {
	if timeout <= 0 {
		return errNonPositiveTimeout(timeout)
	}
	fmt.Fprintf(errW, "waiting for run %s to be running (timeout %s)\n", runID, timeout)
	deadline := time.Now().Add(timeout)
	waitCtx, cancel := context.WithDeadlineCause(ctx, deadline, errWaitTimeout)
	defer cancel()
	timedOut := func(last types.RunState) error {
		return &exitError{code: 124, err: fmt.Errorf("timed out after %s waiting for run %s (last state %s)", timeout, runID, last)}
	}
	consecutiveErrs := 0
	var last types.RunState
	lastAhead := -1
	for {
		run, err := c.GetRun(waitCtx, runID)
		if err != nil {
			if context.Cause(waitCtx) == errWaitTimeout {
				return timedOut(last)
			}
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				return fmt.Errorf("polling run %s failed %d times in a row: %w", runID, consecutiveErrs, err)
			}
		} else {
			consecutiveErrs = 0
			last = run.State
			lastAhead = reportQueuePlace(errW, run, lastAhead)
			switch {
			case run.State == types.RunRunning, run.State == types.RunCompleted:
				fmt.Fprintf(errW, "run %s is %s\n", runID, strings.ToLower(string(run.State)))
				return nil
			case run.State.IsTerminal():
				msg := fmt.Sprintf("run %s ended before it was running: state %s", runID, run.State)
				if sentence := queueEndSentence(run); sentence != "" {
					msg += ": " + sentence
				} else if run.State == types.RunFailed {
					if reason := runFailureReason(waitCtx, c, runID); reason != "" {
						msg += ": " + reason
					}
				}
				code := 2
				if run.State == types.RunFailed {
					code = 1
				}
				return &exitError{code: code, err: errors.New(msg)}
			}
		}
		if time.Now().After(deadline) {
			return timedOut(last)
		}
		select {
		case <-waitCtx.Done():
			if context.Cause(waitCtx) == errWaitTimeout {
				return timedOut(last)
			}
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

func workloadDiffCmd(client clientFn) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "diff -f FILE",
		Short: "Show how the workloads in a file differ from the stored ones",
		Long: "Read each workload through the organisation's door and compare it with the stored workload of the\n" +
			"same name, as a unified diff of both written as YAML. Exits 0 when every workload is equal, 1 when\n" +
			"any differs or is not stored yet, and 2 or more on an error, as `diff` and `kubectl diff` do.\n" +
			"Writes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			docs, err := readWorkloadDocs(cmd, file)
			if err != nil {
				return diffError(err)
			}
			c := client()
			differs := 0
			for _, d := range docs {
				_, text, err := diffWorkload(cmd.Context(), cmd.ErrOrStderr(), c, d)
				if err != nil {
					return diffError(err)
				}
				if text != "" {
					differs++
					fmt.Fprint(cmd.OutOrStdout(), text)
				}
			}
			if differs > 0 {
				return &exitError{code: 1, err: fmt.Errorf("%d workload(s) differ from the stored ones", differs)}
			}
			return nil
		},
	}
	fileFlag(cmd, &file)
	return cmd
}

// diffError keeps exit 1 for "differs": an error that would exit 1 (a file that does not parse, a
// spec the door refuses) exits 2, and an API or connection error keeps its own code.
func diffError(err error) error {
	if exitCodeFor(err) == 1 {
		return &exitError{code: 2, err: err}
	}
	return err
}

func workloadDeleteCmd(client clientFn) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "delete -f FILE",
		Short: "End the run of each workload in a file and delete the workload",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			docs, err := readWorkloadDocs(cmd, file)
			if err != nil {
				return err
			}
			c := client()
			for _, d := range docs {
				if err := c.DeleteWorkload(cmd.Context(), d.Name, ""); err != nil {
					return fmt.Errorf("workload/%s: %w", d.Name, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "workload/%s deleted\n", d.Name)
			}
			return nil
		},
	}
	fileFlag(cmd, &file)
	return cmd
}

// workloadDryRunReport is one workload's entry of `apply --dry-run --json`.
type workloadDryRunReport struct {
	Name      string              `json:"name"`
	Differs   bool                `json:"differs"`
	Diff      string              `json:"diff,omitempty"`
	Preflight sdk.PreflightResult `json:"preflight"`
}

// workloadDryRun is `apply --dry-run`: the diff, then the run request the apply would make, checked
// by the preflight. Nothing is stored and nothing starts.
func workloadDryRun(cmd *cobra.Command, c *sdk.Client, docs []sdk.WorkloadDocument, asJSON bool) error {
	reports := make([]workloadDryRunReport, 0, len(docs))
	for _, d := range docs {
		norm, text, err := diffWorkload(cmd.Context(), cmd.ErrOrStderr(), c, d)
		if err != nil {
			return err
		}
		req, err := sdk.RunDocumentRequest(norm.Intent)
		if err != nil {
			return fmt.Errorf("workload/%s: %w", d.Name, err)
		}
		req.Title, req.Description, req.RunIn = norm.Name, norm.Description, norm.RunIn
		if asJSON {
			pf, err := c.Preflight(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("workload/%s: %w", d.Name, err)
			}
			reports = append(reports, workloadDryRunReport{Name: d.Name, Differs: text != "", Diff: text, Preflight: pf})
			continue
		}
		if text == "" {
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s unchanged\n", d.Name)
		} else {
			fmt.Fprint(cmd.OutOrStdout(), text)
		}
		if err := printPreflight(cmd.Context(), cmd.OutOrStdout(), c, req, false); err != nil {
			return fmt.Errorf("workload/%s: %w", d.Name, err)
		}
	}
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), reports)
	}
	return nil
}

// diffWorkload normalises the file's workload through the door (as run_file.go does for a Run
// document), reads the stored one, and returns the normalised workload with a unified diff of
// stored against file as YAML. The diff is empty when they are equal; a workload not stored yet
// differs from nothing.
func diffWorkload(ctx context.Context, errW io.Writer, c *sdk.Client, d sdk.WorkloadDocument) (sdk.WorkloadDocument, string, error) {
	src, err := json.Marshal(sdk.TemplateDocument{APIVersion: sdk.TemplateDocumentVersion, Kind: sdk.RunDocumentKind, Intent: d.Intent})
	if err != nil {
		return d, "", err
	}
	res, err := c.ImportTemplate(ctx, sdk.TemplateImportRequest{Format: sdk.TemplateFormatJSON, Source: string(src), Kind: sdk.RunDocumentKind})
	if err != nil {
		return d, "", fmt.Errorf("workload/%s: %w", d.Name, err)
	}
	if res.Document == nil || len(res.Diagnostics) > 0 {
		return d, "", fmt.Errorf("workload/%s: %w", d.Name, printTemplateImport(errW, res))
	}
	d.Intent = res.Document.Intent
	want, err := workloadYAML(d)
	if err != nil {
		return d, "", err
	}
	var have []string
	from := "workload/" + d.Name + " (not stored)"
	stored, err := c.GetWorkload(ctx, d.Name, "")
	switch {
	case err == nil:
		text, err := workloadYAML(sdk.WorkloadDocument{APIVersion: sdk.TemplateDocumentVersion, Kind: sdk.WorkloadDocumentKind, Name: stored.Name,
			Description: stored.Description, RunIn: stored.RunIn, RestartKey: stored.RestartKey, Intent: stored.Intent})
		if err != nil {
			return d, "", err
		}
		have, from = splitLines(text), "workload/"+d.Name+" (stored)"
	case !isWorkloadMissing(err):
		return d, "", fmt.Errorf("workload/%s: %w", d.Name, err)
	}
	return d, unifiedDiff(from, "workload/"+d.Name+" (file)", have, splitLines(want)), nil
}

func isWorkloadMissing(err error) bool {
	var ae *sdk.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// workloadYAML writes the workload as YAML with its keys sorted, so equal workloads read equal.
func workloadYAML(d sdk.WorkloadDocument) (string, error) {
	js, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.UseNumber() // 3600 stays 3600, not 3600.0 or 3.6e+03
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	out, err := yaml.Marshal(v)
	return string(out), err
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLine is one line of an edit script: ' ' kept, '-' only in the stored text, '+' only in the file's.
type diffLine struct {
	op   byte
	text string
}

// diffContext is how many unchanged lines a hunk shows around a change.
const diffContext = 3

// diffCellLimit bounds the table lineDiff fills, and diffSideLimit each side's changed lines (so the
// table size cannot overflow); past either, the changed middle is shown replaced whole.
const (
	diffCellLimit = 16 << 20
	diffSideLimit = 1 << 20
)

// lineDiff is the shortest edit script from a to b. The common head and tail are set aside first,
// so a small change in a long document costs a small table.
func lineDiff(a, b []string) []diffLine {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	var out []diffLine
	for _, s := range a[:head] {
		out = append(out, diffLine{' ', s})
	}
	am, bm := a[head:len(a)-tail], b[head:len(b)-tail]
	n, m := len(am), len(bm)
	if n > diffSideLimit || m > diffSideLimit || n*m > diffCellLimit {
		for _, s := range am {
			out = append(out, diffLine{'-', s})
		}
		for _, s := range bm {
			out = append(out, diffLine{'+', s})
		}
	} else {
		// lcs[i*(m+1)+j] is the longest common subsequence of am[i:] and bm[j:].
		lcs := make([]int32, (n+1)*(m+1))
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
				} else {
					lcs[i*(m+1)+j] = max(lcs[(i+1)*(m+1)+j], lcs[i*(m+1)+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && am[i] == bm[j]:
				out = append(out, diffLine{' ', am[i]})
				i++
				j++
			case j >= m || (i < n && lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]):
				out = append(out, diffLine{'-', am[i]})
				i++
			default:
				out = append(out, diffLine{'+', bm[j]})
				j++
			}
		}
	}
	for _, s := range a[len(a)-tail:] {
		out = append(out, diffLine{' ', s})
	}
	return out
}

// unifiedDiff is a unified diff of a (named from) to b (named to), "" when they are equal. An
// absent side is an empty slice.
func unifiedDiff(from, to string, a, b []string) string {
	if len(a) == 1 && a[0] == "" {
		a = nil
	}
	ops := lineDiff(a, b)
	// aBefore[k] and bBefore[k] count the lines of each side before ops[k].
	aBefore, bBefore := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for k, o := range ops {
		aBefore[k+1], bBefore[k+1] = aBefore[k], bBefore[k]
		if o.op != '+' {
			aBefore[k+1]++
		}
		if o.op != '-' {
			bBefore[k+1]++
		}
	}
	var sb strings.Builder
	for k := 0; k < len(ops); {
		if ops[k].op == ' ' {
			k++
			continue
		}
		start := max(k-diffContext, 0)
		end := k // the last changed op of the hunk
		for next := k + 1; next < len(ops) && next-end <= 2*diffContext+1; next++ {
			if ops[next].op != ' ' {
				end = next
			}
		}
		stop := min(end+diffContext+1, len(ops))
		if sb.Len() == 0 {
			fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
		}
		aStart, aN := aBefore[start]+1, aBefore[stop]-aBefore[start]
		bStart, bN := bBefore[start]+1, bBefore[stop]-bBefore[start]
		if aN == 0 {
			aStart-- // an empty range starts at the line before it
		}
		if bN == 0 {
			bStart--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aN, bStart, bN)
		for _, o := range ops[start:stop] {
			sb.WriteByte(o.op)
			sb.WriteString(o.text)
			sb.WriteByte('\n')
		}
		k = stop
	}
	return sb.String()
}
