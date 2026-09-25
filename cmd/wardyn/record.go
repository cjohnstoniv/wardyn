// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// recordCmd is the Recording Mode CLI: synthesize a reusable least-privilege
// sandbox profile (a saved RunPolicy) from what a run ACTUALLY did.
func recordCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Recording Mode: learn a reusable sandbox profile from a run's activity",
		Long: "Recording Mode learns a tightened RunPolicy ('sandbox profile') from what a run\n" +
			"ACTUALLY did — the egress domains, files, and credentials it used, as captured in the\n" +
			"audit / ground-truth streams. Launch an open (allow-all egress) recording run, let it\n" +
			"work, then `wardyn record synthesize <run-id>` to preview the least-privilege profile or\n" +
			"`wardyn record save <run-id> --name <n>` to save it for reuse by future enforced runs.",
	}

	var synthJSON bool
	synth := &cobra.Command{
		Use:   "synthesize <run-id>",
		Short: "Preview a sandbox profile synthesized from a run's recorded activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			p, err := client().SynthesizeProfile(cmd.Context(), id)
			if err != nil {
				return err
			}
			if synthJSON {
				return emitJSON(cmd.OutOrStdout(), p)
			}
			printProfile(cmd.OutOrStdout(), p)
			return nil
		},
	}
	synth.Flags().BoolVar(&synthJSON, "json", false, "emit the synthesized profile as JSON")

	var name string
	var saveJSON bool
	save := &cobra.Command{
		Use:   "save <run-id> --name <policy-name>",
		Short: "Synthesize a sandbox profile from a run and save it as a named policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			p, err := client().SynthesizeProfile(cmd.Context(), id)
			if err != nil {
				return err
			}
			pol, err := client().CreatePolicy(cmd.Context(), sdk.PolicyRequest{Name: name, Spec: p.Proposed.InlinePolicy})
			if err != nil {
				return err
			}
			if saveJSON {
				return emitJSON(cmd.OutOrStdout(), pol)
			}
			printProfile(cmd.OutOrStdout(), p)
			fmt.Fprintf(cmd.OutOrStdout(), "\nsaved sandbox profile as policy %q (id %s)\n", pol.Name, pol.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "  launch an enforced run with: wardyn run --agent <agent> --policy %s\n", pol.ID)
			return nil
		},
	}
	save.Flags().StringVar(&name, "name", "", "name for the saved policy (required)")
	save.Flags().BoolVar(&saveJSON, "json", false, "emit the saved policy as JSON")
	_ = save.MarkFlagRequired("name")

	var taskJSON bool
	task := &cobra.Command{
		Use:   "task <workspace-id> <task-key>",
		Short: "Record one workspace import task in an OPEN sandbox (learn what it actually uses)",
		Long: "Launches a single named OPEN (allow-all egress) recording sandbox via the workspace\n" +
			"import pipeline. task-key is a free-form name you choose (\"build & test\", \"agent dev\n" +
			"loop\", anything) — not picked from a derived taxonomy. The session idles for\n" +
			"`wardyn attach` and ends with the normal run kill (Done recording). The capture lands\n" +
			"on the workspace when the run terminates.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			wsID, err := parseID("workspace", args[0])
			if err != nil {
				return err
			}
			resp, err := client().RecordWorkspaceTask(cmd.Context(), wsID, args[1])
			if err != nil {
				return err
			}
			if taskJSON {
				return emitJSON(cmd.OutOrStdout(), resp)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "record run %s launched (task %s, mode %s)\n", resp.RecordRunID, resp.TaskKey, resp.Mode)
			if resp.Detail != "" {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+resp.Detail)
			}
			for _, w := range resp.Warnings {
				fmt.Fprintln(cmd.OutOrStdout(), "  warning: "+w)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "  when it completes: `wardyn record synthesize "+resp.RecordRunID+"` for a task profile,")
			fmt.Fprintln(cmd.OutOrStdout(), "  or promote the observed egress from the console's import panel")
			return nil
		},
	}
	task.Flags().BoolVar(&taskJSON, "json", false, "emit the record-run response as JSON")

	cmd.AddCommand(synth, save, task)
	return subcommandGroup(cmd)
}

func printProfile(w io.Writer, p sdk.ProfileResult) {
	fmt.Fprintf(w, "Synthesized sandbox profile (overall risk: %s)\n", orDash(p.OverallRisk))
	spec := p.Proposed.InlinePolicy
	fmt.Fprintf(w, "  min confinement: %s   first-use approval: %s   allow-all egress: %v\n",
		orDash(string(spec.MinConfinementClass)), orDash(string(spec.FirstUseApproval.Normalize())), spec.AllowAllEgress)
	fmt.Fprintf(w, "  allowed domains (%d): %v\n", len(spec.AllowedDomains), spec.AllowedDomains)
	fmt.Fprintf(w, "  eligible grants: %d\n", len(spec.EligibleGrants))
	if len(p.Observations.Domains) > 0 {
		fmt.Fprintln(w, "  observed domains:")
		for _, d := range p.Observations.Domains {
			fmt.Fprintf(w, "    - %s %v\n", d.Host, d.Methods)
		}
	}
	if len(p.Observations.Anomalies) > 0 {
		fmt.Fprintf(w, "  ANOMALIES (%d):\n", len(p.Observations.Anomalies))
		for _, a := range p.Observations.Anomalies {
			fmt.Fprintf(w, "    ! %s\n", a)
		}
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
}

func orDash(s string) string {
	// "null" is the raw json.RawMessage null literal — a grant scope decodes to
	// it when unset, and printing it as a value would read as a real scope.
	if s == "" || s == "null" {
		return "-"
	}
	return s
}
