// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	policyCheckShort = "Check that a candidate policy is no broader than another policy (exit 1 names each field that is)"
	policyCheckLong  = "Check a candidate policy against the policy it must stay within: a stored policy's id, or a\n" +
		"JSON or YAML file. Each side is a whole policy, a bare spec or a {\"spec\":{...}} body.\n\n" +
		"Exit 0 means every request, credential, size and duration the candidate admits, the other\n" +
		"policy admits too. Exit 1 lists each field that is wider, or that cannot be shown to be no\n" +
		"wider: the check refuses what it cannot prove, so it can fail a candidate that is in fact\n" +
		"no broader, and never passes one that is broader. Exit 1 with no verdict printed, or any\n" +
		"other exit code, means the check did not run.\n\n" +
		"The --within side can also be a run template's id. A template holds only some policy fields\n" +
		"and takes the rest from the policy your runs start from. The server works out the policy a\n" +
		"run of the template would start from for you, and checks the candidate against that."
	policyCheckWithin    = "candidate %s is within %s\n"
	policyCheckWider     = "candidate %s is not within %s. Wider, or not shown to be no wider:\n"
	policyCheckNoField   = "  no single field accounts for it; compare the two policies as wholes\n"
	policyCheckNotWithin = "policy check: the candidate is not within %s"
	policyCheckNoSide    = "no policy or run template you can read has the id %s: %w"
	policyCheckNoServer  = "no policy has the id %s, and this server cannot check against a run template: pass a stored policy's id or a policy file: %w"
	policyCheckEmpty     = "the document holds no policy"
)

// policyCheckResult is what `policy check --json` prints.
type policyCheckResult struct {
	Within    bool                `json:"within"`
	Widenings []composer.Widening `json:"widenings"`
}

// policyCheckCmd answers one question for a pipeline: does the candidate policy admit anything the
// policy it is measured against does not? The verdict is composer.Leq, the rule the control plane
// uses to tell a narrowing from a widening, so "no broader than" has one definition. Leq refuses
// what it cannot prove narrower, so this command can fail a candidate that is no broader and never
// passes one that is.
func policyCheckCmd(client clientFn) *cobra.Command {
	var file, within string
	var asJSON bool
	check := &cobra.Command{
		Use:   "check --within <policy-id|template-id|file> -f <candidate>",
		Short: policyCheckShort,
		Long:  policyCheckLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cand, err := readCheckSide(cmd.InOrStdin(), file)
			if err != nil {
				return fmt.Errorf("candidate %s: %w", file, err)
			}
			res, err := checkWithin(cmd, client, within, cand)
			if err != nil {
				return fmt.Errorf("--within %s: %w", within, err)
			}
			if asJSON {
				if err := emitJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				printPolicyCheck(cmd.OutOrStdout(), file, within, res)
			}
			if !res.Within {
				return &exitError{code: 1, err: fmt.Errorf(policyCheckNotWithin, within)}
			}
			return nil
		},
	}
	check.Flags().StringVarP(&file, "file", "f", "", "the candidate: path to a JSON or YAML policy or spec (use '-' for stdin)")
	check.Flags().StringVar(&within, "within", "", "the policy the candidate must stay within: a stored policy's id, a run template's id, or a path to a JSON or YAML policy or spec")
	check.Flags().BoolVar(&asJSON, "json", false, "emit the verdict and the field list as JSON")
	_ = check.MarkFlagRequired("file")
	_ = check.MarkFlagRequired("within")
	return check
}

// checkWithin is the verdict on cand against the --within side. A file, or a stored policy when the
// value is a UUID, is judged here, and the verdict is Leq's alone: the field list only explains a
// false one. The stored policy is read as the server wrote it and decoded strictly, so a field this
// build does not know refuses the check instead of being dropped and read as an absent restriction.
// A UUID that names no stored policy is tried as a run template, which only the server can judge.
func checkWithin(cmd *cobra.Command, client clientFn, within string, cand types.RunPolicySpec) (policyCheckResult, error) {
	var base types.RunPolicySpec
	id, err := uuid.Parse(within)
	if err != nil {
		base, err = readCheckSide(nil, within)
	} else {
		var raw json.RawMessage
		raw, err = client().GetPolicyDocument(cmd.Context(), id)
		var ae *sdk.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return checkTemplateWithin(cmd, client, id, cand)
		}
		if err == nil {
			base, err = checkSpecFrom(raw)
		}
	}
	if err != nil {
		return policyCheckResult{}, err
	}
	res := policyCheckResult{
		Within:    composer.Leq(cand, base, types.GovernanceLimits{}, types.GovernanceLimits{}),
		Widenings: []composer.Widening{},
	}
	if !res.Within {
		res.Widenings = append(res.Widenings, composer.LeqWidenings(cand, base)...)
	}
	return res, nil
}

// checkTemplateWithin takes the server's verdict on cand against run template id. A template holds
// only some policy fields and takes the rest from the policy of whoever launches it, which only the
// server resolves, so this build keeps no copy of that rule. The template's own 404 means the id names
// neither a policy nor a template the caller can read. A 404 or 405 with no reason, or a server that
// keeps no templates, is a server that cannot check against one. A verdict that says within but names
// a field is read as not within.
func checkTemplateWithin(cmd *cobra.Command, client clientFn, id uuid.UUID, cand types.RunPolicySpec) (policyCheckResult, error) {
	got, err := client().CheckTemplatePolicy(cmd.Context(), id, cand)
	var ae *sdk.APIError
	switch {
	case errors.As(err, &ae) && ae.Reason == "template_not_found":
		return policyCheckResult{}, fmt.Errorf(policyCheckNoSide, id, err)
	case errors.As(err, &ae) && (ae.Reason == "templates_unavailable" ||
		ae.Reason == "" && (ae.Status == http.StatusNotFound || ae.Status == http.StatusMethodNotAllowed)):
		return policyCheckResult{}, fmt.Errorf(policyCheckNoServer, id, err)
	case err != nil:
		return policyCheckResult{}, err
	}
	res := policyCheckResult{Within: got.Within && len(got.Widenings) == 0, Widenings: []composer.Widening{}}
	for _, w := range got.Widenings {
		res.Widenings = append(res.Widenings, composer.Widening(w))
	}
	return res, nil
}

// readCheckSide reads one side from a file, or from in when the path is "-" and in is given.
func readCheckSide(in io.Reader, path string) (types.RunPolicySpec, error) {
	var raw []byte
	var err error
	if path == "-" && in != nil {
		raw, err = io.ReadAll(in)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return types.RunPolicySpec{}, err
	}
	return checkSpecFrom(raw)
}

// checkSpecFrom decodes one whole policy, a bare spec or a {"spec":{...}} body in JSON or YAML, as
// `policy render` does: strictly, so a misspelled field is an error. An empty document is refused:
// read as an empty policy it would be within anything, and a gate must not pass on a file that was
// never written.
func checkSpecFrom(raw []byte) (types.RunPolicySpec, error) {
	doc, err := policyToJSON(raw)
	if err != nil {
		return types.RunPolicySpec{}, err
	}
	if hasSpec(doc) {
		var body struct {
			Spec json.RawMessage `json:"spec"`
		}
		if err := json.Unmarshal(doc, &body); err != nil {
			return types.RunPolicySpec{}, fmt.Errorf("parse policy body: %w", err)
		}
		if err := types.RefuseRepeatedJSONKeysFor(doc, "the policy", &body); err != nil {
			return types.RunPolicySpec{}, fmt.Errorf("parse policy body: %w", err)
		}
		doc = body.Spec
	}
	if t := bytes.TrimSpace(doc); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return types.RunPolicySpec{}, errors.New(policyCheckEmpty)
	}
	return decodeSpecStrict(doc)
}

// printPolicyCheck writes the verdict for a person: one line when the candidate is within, else one
// line per field, a list field followed by the entries that are not within.
func printPolicyCheck(w io.Writer, file, within string, res policyCheckResult) {
	if res.Within {
		fmt.Fprintf(w, policyCheckWithin, file, within)
		return
	}
	fmt.Fprintf(w, policyCheckWider, file, within)
	if len(res.Widenings) == 0 {
		fmt.Fprint(w, policyCheckNoField)
	}
	// A field name and its entries come from the candidate or from a server's answer: neither may start a line.
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, s)
	}
	for _, wd := range res.Widenings {
		if len(wd.Entries) == 0 {
			fmt.Fprintf(w, "  %s\n", clean(wd.Field))
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", clean(wd.Field), clean(strings.Join(wd.Entries, ", ")))
	}
}
