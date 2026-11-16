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
		"A run template cannot be the --within side: it carries only some policy fields and takes\n" +
		"the rest from the policy of whoever launches it."
	policyCheckWithin    = "candidate %s is within %s\n"
	policyCheckWider     = "candidate %s is not within %s. Wider, or not shown to be no wider:\n"
	policyCheckNoField   = "  no single field accounts for it; compare the two policies as wholes\n"
	policyCheckNotWithin = "policy check: the candidate is not within %s"
	policyCheckNoPolicy  = "no policy has the id %s (a run template cannot be the --within side: pass a stored policy's id or a policy file): %w"
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
		Use:   "check --within <policy-id|file> -f <candidate>",
		Short: policyCheckShort,
		Long:  policyCheckLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cand, err := readCheckSide(cmd.InOrStdin(), file)
			if err != nil {
				return fmt.Errorf("candidate %s: %w", file, err)
			}
			base, err := readCheckWithin(cmd, client, within)
			if err != nil {
				return fmt.Errorf("--within %s: %w", within, err)
			}
			// The verdict is Leq's alone. The field list only explains a false one.
			res := policyCheckResult{
				Within:    composer.Leq(cand, base, types.GovernanceLimits{}, types.GovernanceLimits{}),
				Widenings: []composer.Widening{},
			}
			if !res.Within {
				res.Widenings = append(res.Widenings, composer.LeqWidenings(cand, base)...)
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
	check.Flags().StringVar(&within, "within", "", "the policy the candidate must stay within: a stored policy's id, or a path to a JSON or YAML policy or spec")
	check.Flags().BoolVar(&asJSON, "json", false, "emit the verdict and the field list as JSON")
	_ = check.MarkFlagRequired("file")
	_ = check.MarkFlagRequired("within")
	return check
}

// readCheckWithin reads the --within side: a stored policy when the value is a UUID, else a file.
// The stored policy is read as the server wrote it and decoded strictly, so a field this build does
// not know refuses the check instead of being dropped and read as an absent restriction.
func readCheckWithin(cmd *cobra.Command, client clientFn, within string) (types.RunPolicySpec, error) {
	id, err := uuid.Parse(within)
	if err != nil {
		return readCheckSide(nil, within)
	}
	raw, err := client().GetPolicyDocument(cmd.Context(), id)
	var ae *sdk.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return types.RunPolicySpec{}, fmt.Errorf(policyCheckNoPolicy, id, err)
	}
	if err != nil {
		return types.RunPolicySpec{}, err
	}
	return checkSpecFrom(raw)
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
	for _, wd := range res.Widenings {
		if len(wd.Entries) == 0 {
			fmt.Fprintf(w, "  %s\n", wd.Field)
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", wd.Field, strings.Join(wd.Entries, ", "))
	}
}
