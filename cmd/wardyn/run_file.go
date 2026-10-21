// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// run_file.go — `wardyn run --file <doc>`: launch a kind: Run document. The
// document carries the whole run, so no per-field flag stands beside it; a run
// is named with --title and --description, as it is everywhere else.

// runFileFlags are the flags that may stand beside --file: the run's name, when
// it starts, and what to do with the run once it exists.
var runFileFlags = map[string]bool{"file": true, "title": true, "description": true, "wait": true, "timeout": true, "max-wait": true, "json": true, "dry-run": true,
	"start-at": true, "time-zone": true}

// runFileRequest reads the document, has the server's door check it, and maps it
// to the create request: every intent field is the request field of the same
// name, except inline_policy, which must go as policy_overlay so it lays onto
// the caller's own default policy and never replaces it. A document that names
// an experience is a 0.9 request, as with --experience.
func runFileRequest(cmd *cobra.Command, c *sdk.Client, file, title, description string) (sdk.CreateRunRequest, error) {
	var req sdk.CreateRunRequest
	var clash []string
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Changed && !runFileFlags[f.Name] {
			clash = append(clash, "--"+f.Name)
		}
	})
	if len(clash) > 0 {
		return req, fmt.Errorf("--file carries the whole run: remove %s (only --title, --description, --wait, --timeout, --max-wait, --start-at, --time-zone, --json and --dry-run go with it)", strings.Join(clash, ", "))
	}
	format := ""
	if file == "-" {
		format = string(sdk.TemplateFormatYAML) // YAML reads JSON too
	}
	raw, f, err := readTemplateInput(cmd, file, format)
	if err != nil {
		return req, err
	}
	res, err := c.ImportTemplate(cmd.Context(), sdk.TemplateImportRequest{Format: f, Source: string(raw), Kind: sdk.RunDocumentKind})
	if err != nil {
		return req, err
	}
	if res.Document == nil || len(res.Diagnostics) > 0 {
		return req, printTemplateImport(cmd.ErrOrStderr(), res)
	}
	intent := res.Document.Intent
	overlay := intent.Raw("inline_policy")
	intent.Unset("inline_policy")
	if req, err = intent.Request(); err != nil {
		return req, fmt.Errorf("read the run document: %w", err)
	}
	req.PolicyOverlay = overlay
	if req.Experience != "" {
		req.ModeContract = sdk.RunModeContractV09
	}
	req.Title, req.Description = title, description
	return req, nil
}
