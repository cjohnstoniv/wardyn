// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// artifactRedirectPlan carries the dispatch-time artifact-redirect wiring derived
// from the operator-wide site-config (NEVER from sandbox input): per-tool config
// files (delivered to the sandbox via WARDYN_ARTIFACT_CONFIG_B64), go's registry
// env, proxy-side token injections, and the corp hosts that must be TLS-MITM'd so
// a token can be injected on the wire (the sandbox never holds it). A zero plan
// means "no redirect configured" — dispatch is unchanged.
type artifactRedirectPlan struct {
	env        map[string]string       // go GOPROXY/GOSUMDB (env-honoring)
	configB64  string                  // per-tool config files, agent-run materializes
	injections []runner.InjectionGrant // proxy-side token injections (api_key grants)
	mitmHosts  []string                // corp hosts to TLS-MITM for token injection
}

// artifactBaseURLs extracts ecosystem -> base URL (URL-only, no token) from a
// SiteConfig, or nil when no overrides are configured.
func artifactBaseURLs(sc types.SiteConfig) map[string]string {
	if len(sc.ArtifactOverrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(sc.ArtifactOverrides))
	for eco, ov := range sc.ArtifactOverrides {
		out[eco] = ov.BaseURL
	}
	return out
}

// planArtifactRedirect builds the dispatch-time artifact-redirect plan for a run
// from the operator-wide site-config. It:
//   - emits each configured ecosystem's per-tool config (URL-only) as a base64
//     env payload agent-run materializes under $HOME, plus go's GOPROXY/GOSUMDB;
//   - for each corp host WITH a token secret THAT EXISTS, authors a stored-secret
//     api_key grant + injection rule so the token injects proxy-side, and marks
//     the host for TLS-MITM (the injector cannot rewrite an opaque CONNECT).
//
// Config-only (no token, or a token whose secret is absent) still redirects the
// URL — anonymous-read corp repos work without a token, and a dangling token ref
// degrades to config-only rather than failing the run (non-blocking posture).
// Grant creation touches the store; a create failure is audited and that one
// ecosystem is skipped, never aborting the run.
func (s *Server) planArtifactRedirect(ctx context.Context, run types.AgentRun, sc types.SiteConfig) artifactRedirectPlan {
	var plan artifactRedirectPlan
	if len(sc.ArtifactOverrides) == 0 {
		return plan
	}
	files, env := workspacescan.EmitArtifactConfig(artifactBaseURLs(sc))
	if len(env) > 0 {
		plan.env = env
	}
	if len(files) > 0 {
		plan.configB64 = encodeArtifactConfig(files)
	}

	// Which stored secrets exist: token injection degrades to config-only when the
	// referenced secret is absent (never fail the run on a dangling ref).
	present := map[string]bool{}
	if s.cfg.Secrets != nil {
		if names, err := s.cfg.Secrets.List(ctx); err == nil {
			for _, n := range names {
				present[n] = true
			}
		}
	}

	// Dedupe token injection by corp HOST — one Artifactory commonly backs several
	// ecosystems on different paths. Deterministic: iterate ecosystems sorted so a
	// shared host with divergent token refs resolves first-wins stably.
	ecos := make([]string, 0, len(sc.ArtifactOverrides))
	for eco := range sc.ArtifactOverrides {
		ecos = append(ecos, eco)
	}
	sort.Strings(ecos)
	seenHost := map[string]bool{}
	for _, eco := range ecos {
		ov := sc.ArtifactOverrides[eco]
		if ov.TokenSecretRef == "" {
			continue // config-only ecosystem (anonymous read)
		}
		host := strings.ToLower(workspacescan.HostOf(ov.BaseURL))
		if host == "" || seenHost[host] {
			continue
		}
		if !present[ov.TokenSecretRef] {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
				run.ID.String(), "warn", mustJSON(map[string]any{
					"ecosystem": eco, "host": host,
					"detail": "token_secret_ref not found; config-only redirect (no token injected)",
				})))
			continue
		}
		seenHost[host] = true
		grantID := uuid.New()
		scope, _ := json.Marshal(map[string]string{
			"host":        host,
			"header":      "Authorization",
			"format":      "Bearer %s",
			"secret_name": ov.TokenSecretRef,
		})
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: grantID, RunID: run.ID, CreatedAt: s.cfg.Now().UTC(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600},
		}); gerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"ecosystem": eco, "host": host, "error": gerr.Error(),
				})))
			continue
		}
		rule, rerr := injectionRuleFromScope(scope)
		if rerr != nil {
			continue
		}
		plan.injections = append(plan.injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
		plan.mitmHosts = append(plan.mitmHosts, host)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.artifact.redirect",
			run.ID.String(), "success", mustJSON(map[string]any{
				"ecosystem": eco, "host": host, "tls_mitm": true, "secret_name": ov.TokenSecretRef,
				"detail": "corporate registry token injected proxy-side; sandbox never holds it",
			})))
	}
	return plan
}

// encodeArtifactConfig serialises the per-tool config files into the
// WARDYN_ARTIFACT_CONFIG_B64 payload agent-run consumes: newline-delimited
// "<home-relative-path>\t<base64(content)>" records, sorted for determinism.
// base64 carries arbitrary file bytes (XML/TOML/newlines) through the env var and
// out of any shell parsing.
func encodeArtifactConfig(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p)
		b.WriteByte('\t')
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(files[p])))
	}
	return b.String()
}
