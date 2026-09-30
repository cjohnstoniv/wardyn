// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The wording is the approved packet's (S-3..S-10, S-12, S-14..S-23, S-27,
// S-28, S-30, S-33, S-49, S-50). Change it there first.
const (
	runPolicyShort = "Show the policy a run got when it started, as YAML"
	runPolicyLong  = `Shows where a run's policy came from, what Wardyn changed when the run started, and the policy itself as YAML. If you're an admin, you can reuse the YAML as is with "wardyn run --policy-file". Anyone else sees hidden values as <redacted> and fills them in first.`

	runPolicyNotYet = "Wardyn records this run's policy when its sandbox is set up. This run hasn't reached that step."
	runPolicyNever  = "This run stopped before its sandbox was set up, so no policy was applied to it."
	runPolicyOlder  = "This run started before Wardyn recorded each change, so some changes may not be listed."
	runPolicyHidden = "Values shown as <redacted> are hidden from you. Fill them in before using this as a policy."
	runPolicyHeader = "Changed when the run started"
)

// runPolicyCmd returns `wardyn run policy <run-id> [--json]`. The header lines
// are YAML comments, so the output redirected to a file is a policy file: as is
// for an admin, a starting point for anyone else (runPolicyHidden says so).
func runPolicyCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "policy <run-id>",
		Short: runPolicyShort,
		Long:  runPolicyLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("run", args[0])
			if err != nil {
				return err
			}
			c := client()
			view, err := c.GetRunPolicy(cmd.Context(), id)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), view)
			}
			// Never an empty file behind `> p.yaml`: no policy is an error.
			switch view.State {
			case sdk.RunPolicyViewNotYet:
				return errors.New(runPolicyNotYet)
			case sdk.RunPolicyViewNever:
				return errors.New(runPolicyNever)
			}
			body, err := runPolicyYAML(view)
			if err != nil {
				return err
			}
			person := ""
			if slices.ContainsFunc(view.Changes, func(ch sdk.RunPolicyChange) bool { return ch.Cause == "limits" }) {
				person = limitsPerson(cmd, c, id)
			}
			out := cmd.OutOrStdout()
			for _, line := range runPolicyHeaderLines(view, person) {
				fmt.Fprintln(out, "# "+line)
			}
			_, err = out.Write(body)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full response as JSON")
	return cmd
}

// limitsPerson is who the limits belong to when the reader is not that person:
// "" for the run's own creator, else the creator. A failed lookup reads as the
// creator, so the line never claims "your" for somebody else's run.
func limitsPerson(cmd *cobra.Command, c *sdk.Client, id uuid.UUID) string {
	run, err := c.GetRun(cmd.Context(), id)
	if err != nil {
		return ""
	}
	raw, err := c.Me(cmd.Context())
	if err != nil {
		return run.CreatedBy
	}
	var me struct {
		Principal string `json:"principal"`
	}
	if json.Unmarshal(raw, &me) == nil && me.Principal != "" && me.Principal == run.CreatedBy {
		return ""
	}
	return run.CreatedBy
}

// runPolicyHeaderLines is the comment block above the YAML: where the policy
// started, what changed and why, then the two caveats that apply.
func runPolicyHeaderLines(v sdk.RunPolicyView, person string) []string {
	lines := []string{runPolicySourceLine(v.Source)}
	if v.Source.Preset != "" {
		lines = append(lines, fmt.Sprintf("Launched from the preset %q, version %d.", v.Source.Preset, v.Source.PresetVersion))
	}
	if len(v.Changes) > 0 {
		lines = append(lines, runPolicyHeader)
		for _, ch := range v.Changes {
			lines = append(lines, "  "+runPolicyChangeLabel(ch, person)+": "+strings.Join(runPolicyEntries(ch), ", "))
			for _, d := range ch.Detail {
				lines = append(lines, "    "+d)
			}
		}
	}
	if !v.Complete {
		lines = append(lines, runPolicyOlder)
	}
	if v.Redacted {
		lines = append(lines, runPolicyHidden)
	}
	return lines
}

func runPolicySourceLine(s sdk.RunPolicySource) string {
	switch {
	case s.Kind == "stored" && s.Deleted && s.Name != "":
		return fmt.Sprintf("Started from the saved policy %q, which has since been deleted.", s.Name)
	case s.Kind == "stored" && s.Deleted:
		return "Started from a saved policy that has since been deleted."
	case s.Kind == "stored" && s.Name != "":
		return fmt.Sprintf("Started from the saved policy %q.", s.Name)
	case s.Kind == "inline":
		return "Started from a policy written for this run."
	case s.Kind == "default":
		return "Started from your organization's default policy."
	case s.Kind == "profile" && s.Name != "":
		return fmt.Sprintf("Started from the %s governance profile.", s.Name)
	}
	return "Wardyn set this policy for this run."
}

// runPolicyChangeLabel is the heading a cause sits under. person is "" when the
// reader owns the run.
func runPolicyChangeLabel(ch sdk.RunPolicyChange, person string) string {
	switch ch.Cause {
	case "limits":
		if person == "" {
			return "Narrowed to fit your limits"
		}
		return "Narrowed to fit the limits set for " + person
	case "workspace":
		return "Added for the workspace"
	case "source_control":
		return "Added so the run can reach its code"
	case "git_broker":
		return "Routed through Wardyn's GitHub connection"
	case "model_access":
		return "Added so the agent can reach its model"
	case "mirror":
		return "Switched to your organization's package mirror"
	case "profile":
		return fmt.Sprintf("Limited by the %s governance profile", ch.Profile)
	case "restart":
		if ch.At != nil {
			return "Blocked when the run was restarted on " + ch.At.Format("Jan 2, 2006")
		}
		return "Blocked when the run was restarted"
	case "org_disk":
		return "Disk size set from your organization's default"
	}
	return "Set by Wardyn when the run started"
}

// runPolicyEntries lists what a change added, then what it removed (each
// removal marked "-"). A host reads as itself; an entry of any other field is
// named by its field, as "field=entry".
func runPolicyEntries(ch sdk.RunPolicyChange) []string {
	name := func(e string) string {
		if ch.Field == "allowed_domains" || ch.Field == "denied_domains" {
			return e
		}
		return ch.Field + "=" + e
	}
	var out []string
	for _, e := range ch.Added {
		out = append(out, name(e))
	}
	for _, e := range ch.Removed {
		out = append(out, "-"+name(e))
	}
	return out
}

// runPolicyYAML is the spec as a YAML document in the spec's own key order.
// The JSON encoding is parsed as YAML (a superset) into a node tree and its
// flow style cleared, which keeps the order a map round trip would lose.
func runPolicyYAML(v sdk.RunPolicyView) ([]byte, error) {
	if v.Spec == nil {
		return nil, errors.New("the server returned no policy for this run")
	}
	raw, err := json.Marshal(v.Spec)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("render policy as YAML: %w", err)
	}
	clearYAMLStyle(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render policy as YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clearYAMLStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		clearYAMLStyle(c)
	}
}
