// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// policyCmd manages run policies in the control plane. Policies are gated to
// authenticated humans (SSO session or admin token) — dedicated admin-role
// gating is planned, not yet enforced. The server validates every spec before
// persisting it (a bad spec is rejected with HTTP 400). create/update read the
// policy body from a JSON or YAML file; `render` converts either to canonical JSON.
func policyCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage run policies (egress allowlist, confinement floor, eligible grants)",
	}

	var listJSON bool
	var listLimit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List all policies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			policies, err := client().ListPolicies(cmd.Context(), listPageOpts(listLimit)...)
			if err != nil {
				return err
			}
			if listJSON {
				return emitJSON(policies)
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tNAME\tMIN_CC\tFIRST_USE\tGRANTS\tUPDATED")
			for _, p := range policies {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
					p.ID, p.Name, p.Spec.MinConfinementClass,
					p.Spec.FirstUseApproval.Normalize(), len(p.Spec.EligibleGrants),
					p.UpdatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit raw JSON")
	list.Flags().IntVar(&listLimit, "limit", 0, "max rows to return (0 = server default page)")

	get := &cobra.Command{
		Use:   "get <policy-id>",
		Short: "Show a policy's full spec as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("policy", args[0])
			if err != nil {
				return err
			}
			p, err := client().GetPolicy(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emitJSON(p)
		},
	}

	getDefault := &cobra.Command{
		Use:   "default",
		Short: "Show the control plane's configured default (ceiling) policy",
		Long: "Show the control plane's configured default policy — the ceiling applied to any run\n" +
			"created without a policy_id, and the same ceiling a member's inline policy is clamped\n" +
			"against (W14-S1-6: previously unexposed by UI, CLI or API).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := client().GetDefaultPolicy(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(spec)
		},
	}

	var createFile, createName string
	var createJSON bool
	create := &cobra.Command{
		Use:   "create -f <file.json>",
		Short: "Create a policy from a JSON file ({\"name\":..., \"spec\":{...}})",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := readPolicyFile(createFile, createName)
			if err != nil {
				return err
			}
			p, err := client().CreatePolicy(cmd.Context(), body)
			if err != nil {
				return err
			}
			if createJSON {
				return emitJSON(p)
			}
			fmt.Printf("created policy %s (%q, min %s)\n", p.ID, p.Name, p.Spec.MinConfinementClass)
			return nil
		},
	}
	create.Flags().StringVarP(&createFile, "file", "f", "", "path to a JSON or YAML policy body (use '-' for stdin)")
	create.Flags().StringVar(&createName, "name", "", "policy name (overrides/supplies the name in the file)")
	create.Flags().BoolVar(&createJSON, "json", false, "emit the created policy as JSON")
	_ = create.MarkFlagRequired("file")

	var updateFile, updateName string
	var updateJSON bool
	update := &cobra.Command{
		Use:   "update <policy-id> -f <file.json>",
		Short: "Replace a policy's name and spec from a JSON file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("policy", args[0])
			if err != nil {
				return err
			}
			body, err := readPolicyFile(updateFile, updateName)
			if err != nil {
				return err
			}
			p, err := client().UpdatePolicy(cmd.Context(), id, body)
			if err != nil {
				return err
			}
			if updateJSON {
				return emitJSON(p)
			}
			fmt.Printf("updated policy %s (%q, min %s)\n", p.ID, p.Name, p.Spec.MinConfinementClass)
			return nil
		},
	}
	update.Flags().StringVarP(&updateFile, "file", "f", "", "path to a JSON or YAML policy body (use '-' for stdin)")
	update.Flags().StringVar(&updateName, "name", "", "policy name (overrides/supplies the name in the file)")
	update.Flags().BoolVar(&updateJSON, "json", false, "emit the updated policy as JSON")
	_ = update.MarkFlagRequired("file")

	del := &cobra.Command{
		Use:   "delete <policy-id>",
		Short: "Delete a policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("policy", args[0])
			if err != nil {
				return err
			}
			if err := client().DeletePolicy(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Printf("policy %s deleted\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(list, get, getDefault, create, update, del, policyRenderCmd())
	return cmd
}

// policyRenderCmd converts a JSON or YAML policy (full body or bare spec) to
// canonical JSON. Purely local — it never talks to the control plane; its job is
// to surface a bad spec at authoring time instead of at launch, so it decodes
// strictly (DisallowUnknownFields), mirroring the server's validator.
func policyRenderCmd() *cobra.Command {
	var renderFile string
	render := &cobra.Command{
		Use:   "render -f <file>",
		Short: "Convert a JSON or YAML policy spec to canonical JSON (strict: a misspelled field fails here, not at launch)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var raw []byte
			var err error
			if renderFile == "-" {
				raw, err = io.ReadAll(os.Stdin)
			} else {
				raw, err = os.ReadFile(renderFile)
			}
			if err != nil {
				return fmt.Errorf("read policy file: %w", err)
			}
			jsonBytes, err := policyToJSON(raw)
			if err != nil {
				return err
			}
			// Accept a bare spec or a {"spec":{...}} body; isolate the spec.
			specBytes := jsonBytes
			if hasSpec(jsonBytes) {
				var body struct {
					Spec json.RawMessage `json:"spec"`
				}
				if err := json.Unmarshal(jsonBytes, &body); err != nil {
					return fmt.Errorf("parse policy body: %w", err)
				}
				specBytes = body.Spec
			}
			// Strict decode so an unknown/misspelled field is caught locally,
			// mirroring the server's DisallowUnknownFields validator.
			spec, err := decodeSpecStrict(specBytes)
			if err != nil {
				return fmt.Errorf("invalid RunPolicySpec: %w", err)
			}
			out, err := json.MarshalIndent(spec, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(out))
			return nil
		},
	}
	render.Flags().StringVarP(&renderFile, "file", "f", "", "path to a JSON or YAML policy or spec (use '-' for stdin)")
	_ = render.MarkFlagRequired("file")
	return render
}

// readPolicyFile parses a JSON policy body from path ("-" means stdin). The file
// may contain a full body ({"name":..., "spec":{...}}) or a bare spec object;
// when the top-level "spec" key is absent the whole document is treated as the
// spec. A non-empty nameOverride always wins over any name in the file. The
// enclosing body is parsed loosely (a `policy get --json` dump carries stray
// top-level keys like "id"/"updated_at" that a round-trip edit should tolerate),
// but the spec itself is decoded STRICT — see decodeSpecStrict — so a
// misspelled/unknown spec field fails here, at authoring time, instead of
// silently vanishing into a policy the operator believes enforces it.
func readPolicyFile(path, nameOverride string) (sdk.PolicyRequest, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return sdk.PolicyRequest{}, fmt.Errorf("read policy file: %w", err)
	}
	// Accept JSON or YAML: normalize to canonical JSON so the shape probe and the
	// unmarshals below (and the server's strict validator) all see one format.
	if raw, err = policyToJSON(raw); err != nil {
		return sdk.PolicyRequest{}, fmt.Errorf("parse policy file: %w", err)
	}

	var body sdk.PolicyRequest
	specBytes := raw
	if hasSpec(raw) {
		// Full body shape: pull out the name loosely and isolate the spec bytes
		// for strict decoding below.
		var probe struct {
			Name string          `json:"name"`
			Spec json.RawMessage `json:"spec"`
		}
		if perr := json.Unmarshal(raw, &probe); perr != nil {
			return sdk.PolicyRequest{}, fmt.Errorf("parse policy body: %w", perr)
		}
		body.Name = probe.Name
		specBytes = probe.Spec
	}
	spec, serr := decodeSpecStrict(specBytes)
	if serr != nil {
		return sdk.PolicyRequest{}, fmt.Errorf("parse policy JSON: %w", serr)
	}
	body.Spec = spec

	if nameOverride != "" {
		body.Name = nameOverride
	}
	if body.Name == "" {
		return sdk.PolicyRequest{}, fmt.Errorf("policy name is required (set \"name\" in the file or pass --name)")
	}
	return body, nil
}

// decodeSpecStrict decodes canonical JSON into a RunPolicySpec, rejecting any
// field the type doesn't recognize (DisallowUnknownFields) — the same shape as
// the server's own strict validator. Shared by readPolicyFile (create/update),
// the --policy-file branch of `run`, and policyRenderCmd, so a misspelled spec
// field fails identically wherever it's authored.
func decodeSpecStrict(raw []byte) (types.RunPolicySpec, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec types.RunPolicySpec
	if err := dec.Decode(&spec); err != nil {
		return types.RunPolicySpec{}, err
	}
	return spec, nil
}

// hasSpec reports whether the JSON document has a top-level "spec" key.
func hasSpec(raw []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, ok := probe["spec"]
	return ok
}
