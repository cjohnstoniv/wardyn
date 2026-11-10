// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
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
			"`wardyn record save <run-id> --name <n>` to save it for reuse by future enforced runs\n" +
			"(`--template` saves it as your own template, which a member may do).",
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
	var saveJSON, asTemplate bool
	save := &cobra.Command{
		Use:   "save <run-id> --name <policy-name>",
		Short: "Synthesize a sandbox profile from a run and save it as a named policy",
		Long: "Saves the synthesized profile as a stored policy, which only an administrator may write.\n" +
			"With --template it is saved as your own template instead: the profile's network and\n" +
			"credential rules, laid onto the policy a run starts from and bounded by your own ceiling\n" +
			"when a run uses it. It never writes a shared policy or another person's template.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			p, err := client().SynthesizeProfile(cmd.Context(), id)
			if err != nil {
				return err
			}
			if asTemplate {
				doc, err := profileTemplateDocument(p.Proposed.InlinePolicy)
				if err != nil {
					return err
				}
				tpl, err := client().SaveTemplate(cmd.Context(), uuid.Nil, sdk.TemplateSaveRequest{Scope: sdk.TemplateScopePerson, Name: name, Document: doc})
				if err != nil {
					return err
				}
				if saveJSON {
					return emitJSON(cmd.OutOrStdout(), tpl)
				}
				printProfile(cmd.OutOrStdout(), p)
				fmt.Fprintf(cmd.OutOrStdout(), "\nsaved sandbox profile as your template %q (id %s)\n", tpl.Name, tpl.ID)
				return nil
			}
			pol, err := client().CreatePolicy(cmd.Context(), sdk.PolicyRequest{Name: name, Spec: p.Proposed.InlinePolicy})
			if err != nil {
				return templateHint(err)
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
	save.Flags().BoolVar(&saveJSON, "json", false, "emit the saved policy or template as JSON")
	save.Flags().BoolVar(&asTemplate, "template", false, "save as your own template, not a stored policy (a member may; the policy is an administrator's)")
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

// templateHint adds the --template remedy to a refused policy save. The route's own reason says it was
// the admin-only gate ("admin_surface"), and then a member is told plainly; a 403 that names another
// reason (governance, an admin-only write) is left alone, and one with no reason gets the conditional.
func templateHint(err error) error {
	var ae *sdk.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		return err
	}
	switch ae.Reason {
	case "admin_surface":
		return fmt.Errorf("%w. Members can save it as their own template with --template", err)
	case "":
		return fmt.Errorf("%w. If you are a member, you can save it as your own template with --template", err)
	}
	return err
}

// recordedPolicyKeys are the inline_policy keys a recording decides, and so the only ones a saved
// template carries (internal/recordmode Synthesize sets exactly these). A template lays each present
// key onto the person's source policy and keeps the source's value for a key it leaves out, so a
// narrowing result must be written even when it is empty or false: an absent eligible_grants would
// keep every grant the source policy holds, and an absent allowed_domains its whole allowlist.
// Every other key is left out so it still inherits: denied_domains, resources and the rest are not
// something a recording can know. allowed_methods is kept only when the proposal lists methods
// (the clamp fills it from the person's ceiling): the synthesizer leaves it empty by design, and an
// empty list in a template means every method, which would widen a source policy that limits them.
var recordedPolicyKeys = []string{
	"allowed_domains", "allow_all_egress", "first_use_approval", "allowed_methods",
	"min_confinement_class", "eligible_grants", "auto_stop_after_sec",
}

// recordedEmptyKeys are the recorded keys whose empty value is itself the decision, with the
// value to write when the proposal carries none: nothing reachable, no allow-all, no grant.
var recordedEmptyKeys = map[string]json.RawMessage{
	"allowed_domains":  json.RawMessage("[]"),
	"allow_all_egress": json.RawMessage("false"),
	"eligible_grants":  json.RawMessage("[]"),
}

// profileTemplateDocument is the personal template a synthesized profile becomes: a partial
// template whose inline_policy holds the recorded keys (recordedPolicyKeys) and no others.
func profileTemplateDocument(spec sdk.RunPolicySpec) (sdk.TemplateDocument, error) {
	// A grant with no scope marshals "scope": null, which the template door refuses as a value. It names
	// no credential to keep, so it is left out (the narrower result) rather than failing the whole save.
	spec.EligibleGrants = slices.DeleteFunc(slices.Clone(spec.EligibleGrants), func(g sdk.GrantSpec) bool {
		scope := strings.TrimSpace(string(g.Scope))
		return scope == "" || scope == "null"
	})
	body, err := json.Marshal(spec)
	if err != nil {
		return sdk.TemplateDocument{}, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return sdk.TemplateDocument{}, err
	}
	keys := map[string]json.RawMessage{}
	for _, k := range recordedPolicyKeys {
		v, has := all[k]
		switch string(v) {
		case "null", `""`, "0":
			has = false
		}
		if !has {
			if empty, decided := recordedEmptyKeys[k]; decided {
				keys[k] = empty
			}
			continue
		}
		keys[k] = v
	}
	var intent sdk.TemplateIntent
	if err := intent.Set("inline_policy", keys); err != nil {
		return sdk.TemplateDocument{}, err
	}
	return sdk.TemplateDocument{
		APIVersion: sdk.TemplateDocumentVersion,
		Kind:       sdk.TemplateDocumentKind,
		Coverage:   sdk.TemplateCoveragePartial,
		Intent:     intent,
	}, nil
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
