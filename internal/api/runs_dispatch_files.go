// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Dispatch-time file_secret resolution: a stored secret's VALUE delivered as
// a FILE in the sandbox, the third resident lane beside env_secret (its
// sibling in runs_dispatch_secrets.go, whose rules this one keeps: mask
// before the sandbox can see it, names-only audit, skip a grant that cannot
// be honoured rather than substitute or blank it).
package api

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveFileSecretGrants resolves this run's file_secret grants
// store->sandbox at dispatch: each grant's scope names a stored secret and the
// FILE its value is delivered as, always in runner.ComponentSecretDir — the
// path is built here from a name fileSecretScopeFields admits only without a
// separator, never authored, so no grant can address anywhere else. The file
// rides SandboxSpec.ManagedFiles as AgentOwned: the driver delivers it before
// the agent's main process runs (what each substrate gives is on
// runner.ManagedFile.AgentOwned).
//
// The read is env_secret's, through the same chokepoints: the run identity's
// namespace (runIdentitySubject), owner-only with no operator fallback for an
// owner_only grant (grantReadOwner + secretstore.GrantRead), and every value
// is on the run's masking manifest (maskDispatchValue) before it is put on the
// spec — so before the sandbox exists, since the manifest completes before
// CreateSandbox. Masking is VERBATIM: the value as stored, and the value
// without its trailing line break (a shell's $(cat file) drops it). A base64,
// hex, split or otherwise transformed rendering is not masked.
//
// One run.file_secret.resolve row per grant, carrying the file, the SECRET
// NAME and whose row was read — never the value. A grant that cannot be
// honoured is skipped and the file is ABSENT. A run that carries any
// file_secret grant on a runner that cannot deliver a managed file at all is
// failed instead (ok=false), because every grant would silently go missing.
// A run with no file_secret grant reads nothing and returns (nil, true).
//
// dispatchRun reaches it through completeMaskManifestWithFileSecrets.
func (s *Server) resolveFileSecretGrants(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec) (files []runner.ManagedFile, ok bool) {
	if !slices.ContainsFunc(policy.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantFileSecret }) {
		return nil, true
	}
	if !s.fileSecretRunnerDelivers(ctx, run) {
		return nil, false
	}
	delivered := map[string]bool{}
	for _, g := range policy.EligibleGrants {
		if g.Kind != types.GrantFileSecret {
			continue
		}
		file, secretName, err := fileSecretScopeFields(g.Scope)
		skip := ""
		switch {
		case err != nil:
			skip = "scope invalid: " + err.Error()
		case nameSinkReservedSecret(secretName):
			skip = "references a reserved platform-internal secret name"
		case secretName == bedrockAPIKeySecret:
			skip = "the Bedrock API key is injected proxy-side only and never written into the sandbox"
		case s.cfg.Secrets == nil:
			skip = "no secret store configured"
		case delivered[file]:
			skip = "another file_secret grant on this run already delivers this file"
		}
		scope := ""
		if skip == "" {
			owner := grantReadOwner(runIdentitySubject(ctx, run.CreatedBy), g.OwnerOnly, run.OperatorOwned)
			gctx, row := secretstore.GrantRead(ctx, g.OwnerOnly)
			val, gerr := s.cfg.Secrets.For(owner).Get(secretstore.WithPurpose(gctx, secretstore.PurposeDispatch), secretName)
			scope = row.Scope()
			switch {
			case g.OwnerOnly && errors.Is(gerr, secretstore.ErrNotFound):
				skip = "the grant is owner_only and the run's owner has no secret of that name of their own"
			case gerr != nil || len(val) == 0:
				skip = "secret could not be resolved"
			case s.maskDispatchValue(ctx, run.ID, fileSecretRenderings(val)...) != nil:
				skip = "the run's masking manifest could not record the value"
			default:
				delivered[file] = true
				files = append(files, runner.ManagedFile{
					Path: runner.ComponentSecretDir + "/" + file, Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: val,
				})
			}
		}
		data := map[string]any{"file": file, "secret_name": secretName}
		if scope != "" {
			data["secret_scope"] = scope
		}
		outcome := "success"
		if skip != "" {
			outcome, data["reason"] = "failure", skip
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.file_secret.resolve",
			run.ID.String(), outcome, mustJSON(data)))
	}
	return files, true
}

// completeMaskManifestWithFileSecrets is the file lane's place in dispatchRun:
// the last values the run receives, resolved onto spec immediately before the
// run's masking manifest completes over them, so the manifest is whole when
// the sandbox is created. false means the run has been failed.
func (s *Server) completeMaskManifestWithFileSecrets(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, spec *runner.SandboxSpec) bool {
	files, ok := s.resolveFileSecretGrants(ctx, run, policy)
	if !ok {
		return false
	}
	spec.ManagedFiles = append(spec.ManagedFiles, files...)
	return s.completeMaskManifest(ctx, run)
}

// fileSecretRenderings is what a delivered file's value is masked as: the
// bytes as stored, plus those bytes without a trailing line break when that
// differs — the form a shell's command substitution reads the file into.
func fileSecretRenderings(val []byte) [][]byte {
	if trimmed := bytes.TrimRight(val, "\r\n"); len(trimmed) != len(val) {
		return [][]byte{val, trimmed}
	}
	return [][]byte{val}
}

// fileSecretRunnerDelivers fails the run closed, with its run.create row,
// when the runner cannot deliver a file or cannot say whether it can: decided
// here, before any value is read, as the orchestrator decides managed files
// before routing (a driver's own refusal is the backstop, not the gate).
func (s *Server) fileSecretRunnerDelivers(ctx context.Context, run types.AgentRun) bool {
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err == nil && caps.ManagedFiles {
		return true
	}
	data := map[string]any{"reason": "managed_files_unsupported"}
	hint := "This run was not launched: it delivers a secret as a file, and its runner cannot place one in the sandbox"
	if err != nil {
		hint = "This run was not launched: it delivers a secret as a file, and whether its runner can place one in the sandbox is unknown"
		data["error"] = err.Error()
	} else {
		data["error"] = "runner " + caps.Driver + " does not deliver managed files"
	}
	s.failAndRevoke(ctx, run.ID, types.RunStarting, hint)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(data)))
	return false
}
