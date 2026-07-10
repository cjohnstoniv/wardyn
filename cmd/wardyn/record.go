// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/spf13/cobra"
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

	synth := &cobra.Command{
		Use:   "synthesize <run-id>",
		Short: "Preview a sandbox profile synthesized from a run's recorded activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := client().synthesizeProfile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printProfile(p)
			return nil
		},
	}

	var name string
	save := &cobra.Command{
		Use:   "save <run-id> --name <policy-name>",
		Short: "Synthesize a sandbox profile from a run and save it as a named policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			p, err := client().synthesizeProfile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printProfile(p)
			pol, err := client().createPolicy(cmd.Context(), policyBody{Name: name, Spec: p.Proposed.InlinePolicy})
			if err != nil {
				return err
			}
			fmt.Printf("\nsaved sandbox profile as policy %q (id %s)\n", pol.Name, pol.ID)
			fmt.Printf("  launch an enforced run with: wardyn run --agent <agent> --policy %s\n", pol.ID)
			return nil
		},
	}
	save.Flags().StringVar(&name, "name", "", "name for the saved policy (required)")

	task := &cobra.Command{
		Use:   "task <workspace-id> <task-key>",
		Short: "Record one workspace import task in an OPEN sandbox (learn what it actually uses)",
		Long: "Launches the task's open (allow-all egress) recording sandbox via the workspace import\n" +
			"pipeline. Task keys come from the workspace's derived record_tasks (build/test/lint/setup/\n" +
			"custom). The session idles for `wardyn attach` and ends with the normal run kill\n" +
			"(Done recording). The capture lands on the workspace when the run terminates.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client().recordWorkspaceTask(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("record run %s launched (task %s, mode %s)\n", resp.RecordRunID, resp.TaskKey, resp.Mode)
			if resp.Detail != "" {
				fmt.Println("  " + resp.Detail)
			}
			for _, w := range resp.Warnings {
				fmt.Println("  warning: " + w)
			}
			fmt.Println("  when it completes: `wardyn record synthesize " + resp.RecordRunID + "` for a task profile,")
			fmt.Println("  or promote the observed egress from the console's import panel")
			return nil
		},
	}

	cmd.AddCommand(synth, save, task)
	return cmd
}

func printProfile(p profileResp) {
	fmt.Printf("Synthesized sandbox profile (overall risk: %s)\n", orDash(p.OverallRisk))
	spec := p.Proposed.InlinePolicy
	fmt.Printf("  min confinement: %s   first-use approval: %s   allow-all egress: %v\n",
		orDash(string(spec.MinConfinementClass)), orDash(string(spec.FirstUseApproval.Normalize())), spec.AllowAllEgress)
	fmt.Printf("  allowed domains (%d): %v\n", len(spec.AllowedDomains), spec.AllowedDomains)
	fmt.Printf("  eligible grants: %d\n", len(spec.EligibleGrants))
	if len(p.Observations.Domains) > 0 {
		fmt.Println("  observed domains:")
		for _, d := range p.Observations.Domains {
			fmt.Printf("    - %s %v\n", d.Host, d.Methods)
		}
	}
	if len(p.Observations.Anomalies) > 0 {
		fmt.Printf("  ANOMALIES (%d):\n", len(p.Observations.Anomalies))
		for _, a := range p.Observations.Anomalies {
			fmt.Printf("    ! %s\n", a)
		}
	}
	for _, w := range p.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
