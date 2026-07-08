// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// policyCmd manages run policies in the control plane. Policies are admin-gated
// config: the server validates every spec before persisting it (a bad spec is
// rejected with HTTP 400). create/update read the policy body from a JSON file.
func policyCmd(client func() *apiClient) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage run policies (egress allowlist, confinement floor, eligible grants)",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List all policies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			policies, err := client().listPolicies(cmd.Context())
			if err != nil {
				return err
			}
			tw := newTab()
			fmt.Fprintln(tw, "ID\tNAME\tMIN_CC\tFIRST_USE\tGRANTS\tUPDATED")
			for _, p := range policies {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
					short(p.ID.String()), p.Name, p.Spec.MinConfinementClass,
					p.Spec.FirstUseApproval.Normalize(), len(p.Spec.EligibleGrants),
					p.UpdatedAt.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}

	get := &cobra.Command{
		Use:   "get <policy-id>",
		Short: "Show a policy's full spec as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := client().getPolicy(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(p)
		},
	}

	var createFile, createName string
	create := &cobra.Command{
		Use:   "create -f <file.json>",
		Short: "Create a policy from a JSON file ({\"name\":..., \"spec\":{...}})",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := readPolicyFile(createFile, createName)
			if err != nil {
				return err
			}
			p, err := client().createPolicy(cmd.Context(), body)
			if err != nil {
				return err
			}
			fmt.Printf("created policy %s (%q, min %s)\n", p.ID, p.Name, p.Spec.MinConfinementClass)
			return nil
		},
	}
	create.Flags().StringVarP(&createFile, "file", "f", "", "path to a JSON policy body (use '-' for stdin)")
	create.Flags().StringVar(&createName, "name", "", "policy name (overrides/supplies the name in the file)")
	_ = create.MarkFlagRequired("file")

	var updateFile, updateName string
	update := &cobra.Command{
		Use:   "update <policy-id> -f <file.json>",
		Short: "Replace a policy's name and spec from a JSON file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readPolicyFile(updateFile, updateName)
			if err != nil {
				return err
			}
			p, err := client().updatePolicy(cmd.Context(), args[0], body)
			if err != nil {
				return err
			}
			fmt.Printf("updated policy %s (%q, min %s)\n", p.ID, p.Name, p.Spec.MinConfinementClass)
			return nil
		},
	}
	update.Flags().StringVarP(&updateFile, "file", "f", "", "path to a JSON policy body (use '-' for stdin)")
	update.Flags().StringVar(&updateName, "name", "", "policy name (overrides/supplies the name in the file)")
	_ = update.MarkFlagRequired("file")

	del := &cobra.Command{
		Use:   "delete <policy-id>",
		Short: "Delete a policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client().deletePolicy(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("policy %s deleted\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(list, get, create, update, del)
	return cmd
}

// readPolicyFile parses a JSON policy body from path ("-" means stdin). The file
// may contain a full body ({"name":..., "spec":{...}}) or a bare spec object;
// when the top-level "spec" key is absent the whole document is treated as the
// spec. A non-empty nameOverride always wins over any name in the file. The
// server validates the spec, so we only do light structural parsing here.
func readPolicyFile(path, nameOverride string) (policyBody, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return policyBody{}, fmt.Errorf("read policy file: %w", err)
	}

	// First try the full body shape.
	var body policyBody
	if jerr := json.Unmarshal(raw, &body); jerr == nil && hasSpec(raw) {
		// Document carried a "spec" key: trust the parsed body.
	} else {
		// Treat the whole document as a bare spec.
		body = policyBody{}
		if serr := json.Unmarshal(raw, &body.Spec); serr != nil {
			return policyBody{}, fmt.Errorf("parse policy JSON: %w", serr)
		}
	}
	if nameOverride != "" {
		body.Name = nameOverride
	}
	if body.Name == "" {
		return policyBody{}, fmt.Errorf("policy name is required (set \"name\" in the file or pass --name)")
	}
	return body, nil
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
