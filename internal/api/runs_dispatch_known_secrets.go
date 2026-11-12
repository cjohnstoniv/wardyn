// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With detect_secrets on, the known-secret check covers every secret dispatch
// places in the run: an env_secret value, a file_secret value and a component's
// secret, which rides those two. There is no setting for it: a policy that checks
// secrets checks the ones the run holds. A value that stays on a person's own
// runner, or is minted at run time, is not placed by dispatch and is not added. The
// check is a whole-value match: single-line values, and multi-line values only when
// pasted verbatim.

// placedSecretForms is what a placed value is checked as: the whole value, never
// one line of it. As stored; without its trailing line break; and, for a value with
// a line break inside it, with LF and with CRLF line ends. Values under the masking
// minimum are left out, as the scanner would drop them.
func placedSecretForms(val []byte) []string {
	body := bytes.TrimRight(val, "\r\n")
	forms := []string{string(val), string(body)}
	if bytes.IndexByte(body, '\n') >= 0 {
		lf := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		forms = append(forms, string(lf), string(bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))))
	}
	return slices.DeleteFunc(forms, func(f string) bool { return len(f) < secretmask.MinLen })
}

// addPlacedSecrets adds the forms of vals to the policy's known-secret corpus when
// the policy checks secrets. The inspection block is shared with the policy the
// dispatch was handed, so the forms go on a copy of it and the policy points at the
// copy.
func addPlacedSecrets(policy *types.RunPolicySpec, vals ...[]byte) {
	li := policy.LLMInspection
	if li == nil || !li.DetectSecrets || len(vals) == 0 {
		return
	}
	cp := *li
	cp.WorkspaceSecretValues = slices.Clone(li.WorkspaceSecretValues)
	for _, v := range vals {
		for _, f := range placedSecretForms(v) {
			if !slices.Contains(cp.WorkspaceSecretValues, f) {
				cp.WorkspaceSecretValues = append(cp.WorkspaceSecretValues, f)
			}
		}
	}
	policy.LLMInspection = &cp
}

// placedFileValues is the content of each delivered file whose value the sandbox
// holds: not a runner_resident file, whose content is a placeholder. by[i] is the
// grant that delivered files[i].
func placedFileValues(files []runner.ManagedFile, by []types.GrantSpec, rows []types.CredentialGrant) [][]byte {
	var out [][]byte
	for i, f := range files {
		if _, resident := residentGrantRow(rows, by[i]); !resident {
			out = append(out, f.Content)
		}
	}
	return out
}

// keepFinalRunProxyConfig stores the proxy config again once the file_secret files
// have joined the spec: their values are in the corpus and their placeholders in
// the injections, and a revive rebuilds the proxy from the stored row alone. false
// means the run has been failed.
func (s *Server) keepFinalRunProxyConfig(ctx context.Context, run types.AgentRun, pc runner.ProxyConfig, stopLease func()) bool {
	err := s.keepRunProxyConfig(ctx, run.ID, pc)
	if err == nil {
		return true
	}
	stopLease()
	s.failAndRevoke(ctx, run.ID, types.RunStarting, "the run's proxy config could not be stored")
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
	return false
}
