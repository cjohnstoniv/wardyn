// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// template.go — reusable run templates (a whole or partial run setup a person
// keeps, or an administrator publishes to the organisation or to one group):
//
//	wardyn template list
//	wardyn template show <id> [--revision N]
//	wardyn template save <file> --scope person|org|group [--group ID]
//	wardyn template import <file> [--format json|yaml]
//
// Flags and files are checked here, then the SDK makes the real call. Until the
// server's template store lands, the server answers 501 templates_unavailable.
func templateCmd(client clientFn) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "List, show, save and import run templates",
		Long: "Run templates are reusable run setups: a whole run or a mix of parts such as an\n" +
			"egress allowlist, drives, repositories or component settings. A template is\n" +
			"personal, published to the organisation by an administrator, or published to a\n" +
			"group by that group's administrator. Using one never grants access: the run\n" +
			"still resolves your own policy, credentials, components, pools and drives.\n\n" +
			"Until the server's template store lands, it answers that templates are not\n" +
			"available yet.",
	}
	cmd.AddCommand(templateListCmd(client), templateShowCmd(client), templateSaveCmd(client), templateImportCmd(client))
	return subcommandGroup(cmd)
}

func templateListCmd(client clientFn) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the templates you may use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := client().ListTemplates(cmd.Context())
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), list)
		},
	}
}

func templateShowCmd(client clientFn) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Print one template (--revision for an earlier revision)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID("template", args[0])
			if err != nil {
				return err
			}
			if revision < 0 {
				return fmt.Errorf("--revision must be 1 or more (0 is the current revision)")
			}
			t, err := client().GetTemplate(cmd.Context(), id, revision)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), t)
		},
	}
	cmd.Flags().IntVar(&revision, "revision", 0, "show this revision instead of the current one")
	return cmd
}

func templateSaveCmd(client clientFn) *cobra.Command {
	var scope, groupID, name, description, update string
	var expected int
	cmd := &cobra.Command{
		Use:   "save <file>",
		Short: "Save a template document (a JSON or YAML file, or '-' for stdin) to a scope",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, id, err := templateSaveRequest(cmd, args[0], scope, groupID, name, description, update, expected)
			if err != nil {
				return err
			}
			t, err := client().SaveTemplate(cmd.Context(), id, req)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), t)
		},
	}
	f := cmd.Flags()
	f.StringVar(&scope, "scope", "person", "who the template is for: person, org (administrators) or group (that group's administrators)")
	f.StringVar(&groupID, "group", "", "the group, for --scope group")
	f.StringVar(&name, "name", "", "the template's name (default: the document's own name)")
	f.StringVar(&description, "description", "", "the template's description")
	f.StringVar(&update, "update", "", "update this template id instead of creating a new one; needs --expected-revision")
	f.IntVar(&expected, "expected-revision", 0, "the revision you read; the update is refused if the template has moved on")
	return cmd
}

// templateSaveRequest checks the flags and the document and builds the request.
func templateSaveRequest(cmd *cobra.Command, file, scope, groupID, name, description, update string, expected int) (sdk.TemplateSaveRequest, uuid.UUID, error) {
	var req sdk.TemplateSaveRequest
	switch sdk.TemplateScope(scope) {
	case sdk.TemplateScopePerson, sdk.TemplateScopeOrg:
		if groupID != "" {
			return req, uuid.Nil, fmt.Errorf("--group names a group: use it with --scope group")
		}
	case sdk.TemplateScopeGroup:
		if strings.TrimSpace(groupID) == "" {
			return req, uuid.Nil, fmt.Errorf("--scope group needs --group")
		}
	default:
		return req, uuid.Nil, fmt.Errorf("--scope %q is not person, org or group", scope)
	}
	id := uuid.Nil
	if update != "" {
		var err error
		if id, err = parseID("template", update); err != nil {
			return req, id, err
		}
		if expected < 1 {
			return req, id, fmt.Errorf("--update needs --expected-revision, the revision you read")
		}
		req.ExpectedRevision = &expected
	} else if expected != 0 {
		return req, id, fmt.Errorf("--expected-revision only applies with --update")
	}
	raw, format, err := readTemplateInput(cmd, file, "")
	if err != nil {
		return req, id, err
	}
	jsonDoc := raw
	if format == sdk.TemplateFormatYAML {
		if jsonDoc, err = policyToJSON(raw); err != nil {
			return req, id, fmt.Errorf("parse template YAML: %w", err)
		}
	}
	if err := decodeOneJSONStrict(bytes.NewReader(jsonDoc), &req.Document); err != nil {
		return req, id, fmt.Errorf("parse template document: %w", err)
	}
	req.Scope, req.GroupID, req.Description = sdk.TemplateScope(scope), groupID, description
	if req.Name = strings.TrimSpace(name); req.Name == "" {
		req.Name = strings.TrimSpace(req.Document.Name)
	}
	if req.Name == "" {
		return req, id, fmt.Errorf("a template needs a name: pass --name, or set name in the document")
	}
	return req, id, nil
}

func templateImportCmd(client clientFn) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Check a template document (JSON or YAML, or '-' for stdin) and print it normalised",
		Long: "Ask the server to read a template document and say whether it can become a\n" +
			"draft. Nothing is stored. Every problem is listed with its path and reason, and\n" +
			"the exit status is non-zero when there is one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, f, err := readTemplateInput(cmd, args[0], format)
			if err != nil {
				return err
			}
			res, err := client().ImportTemplate(cmd.Context(), sdk.TemplateImportRequest{Format: f, Source: string(raw)})
			if err != nil {
				return err
			}
			return printTemplateImport(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "json or yaml (default: from the file extension; required for stdin)")
	return cmd
}

// readTemplateInput reads the file ('-' is stdin) and settles its format: the
// --format flag, else the extension.
func readTemplateInput(cmd *cobra.Command, file, format string) ([]byte, sdk.TemplateFormat, error) {
	f := sdk.TemplateFormat(format)
	if format == "" {
		switch strings.ToLower(filepath.Ext(file)) {
		case ".yaml", ".yml":
			f = sdk.TemplateFormatYAML
		case ".json":
			f = sdk.TemplateFormatJSON
		default:
			f = sdk.TemplateFormatJSON
			if file == "-" {
				return nil, "", fmt.Errorf("reading stdin needs --format json or --format yaml")
			}
		}
	}
	if f != sdk.TemplateFormatJSON && f != sdk.TemplateFormatYAML {
		return nil, "", fmt.Errorf("--format %q is not json or yaml", format)
	}
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read template document: %w", err)
	}
	return raw, f, nil
}

// printTemplateImport prints the normalised document, or each diagnostic and an
// error, so a script can branch on the exit status.
func printTemplateImport(w io.Writer, res sdk.TemplateImportResult) error {
	if len(res.Diagnostics) == 0 && res.Document != nil {
		return emitJSON(w, res.Document)
	}
	if len(res.Diagnostics) == 0 {
		return fmt.Errorf("the server returned no document and no diagnostics")
	}
	for _, d := range res.Diagnostics {
		path := d.Path
		if path == "" {
			path = "document"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", path, d.Reason, d.Message)
	}
	return fmt.Errorf("the template document has %d problem(s)", len(res.Diagnostics))
}
