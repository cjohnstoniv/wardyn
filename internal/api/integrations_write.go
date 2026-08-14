// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// integrations_write.go is the write + run/composer resolution half of
// integrations.go, split out (scripts/check-file-size.sh) once the file grew
// past the 1000-line gate: validateIntegrationWrite backs the write endpoints
// (setup_integrations.go); resolveIntegrationRef/defaultAgentRunsIntegration
// back the run-time resolution ladder (llmcred.go); WardynFeaturesBackend
// backs the composer-registry boot derivation (cmd/wardynd/composer.go).
// Everything here builds ON TOP of effectiveIntegrations/capabilitiesFor
// (integrations.go) without changing either.

// genericIntegrationKind reports whether kind is a GENERIC open slug — one
// whose behavior does NOT come from code: the row's own secrets, egress and
// delivery are the whole contract (see types.ClosedIntegrationKinds). The two
// legacy topology slugs are excluded so the legacy-DERIVED artifact_mirror/
// host_proxy rows (legacyIntegrations) keep routing to their bespoke matrices.
// track-b: B1 shim (the topology exception) — dies in B2 with the derivation.
func genericIntegrationKind(kind string) bool {
	return !types.ClosedIntegrationKinds[kind] && kind != "artifact_mirror" && kind != "host_proxy"
}

// legacySortCategory reproduces the pre-base-component category grouping the
// effective-set sort keyed on, from kind alone (all generic kinds collapse
// into one "connection" bucket — the stored category is gone by design).
// track-b: B1 shim, removed in B2/B3 when the surface re-derives its own
// grouping from kind.
func legacySortCategory(kind string) string {
	switch {
	case types.AIProviderKind(kind):
		return "ai_provider"
	case kind == "github_app" || kind == "git_host":
		return "scm_host"
	case kind == "artifact_mirror" || kind == "host_proxy":
		return kind
	default:
		return "connection"
	}
}

// maxIntegrationHosts bounds one integration's host list. Well under the
// per-run 64-host egress cap the requirements surface already warns at, so a
// single integration can never be the thing that blows it.
const maxIntegrationHosts = 32

// validateIntegrationHosts checks the host list against the SAME shape rule
// every operator-supplied policy allowlist entry runs (proxy.ValidDomainEntry
// — exact host, leading-"*." wildcard, optional ":port"), because these
// entries become exactly that: allowlist entries on a granted run.
//
// hasHeader tightens it to BARE EXACT hosts, for one reason with two shapes:
// proxy-side injection resolves through Policy.AllowedExactHost, which consults
// the exact-host set ONLY — deliberately, so a credential can never leak to a
// wildcard-matched host. A wildcard entry ("*.corp.internal") and a
// port-qualified one ("nexus.corp.internal:8443") both compile into other sets
// (allowedWild / allowedExactPort), so neither can ever satisfy it:
//
//   - the wildcard row would open the path and silently never present the
//     credential — a row that looks credentialed and isn't;
//   - the port-qualified row is worse than silent. buildInjector REFUSES an
//     injection rule whose host misses the exact allowlist, and that refusal is
//     a hard proxy startup failure, so the run is bricked rather than
//     under-credentialed.
//
// Reject both at write time and say why. A host with a port or a wildcard is
// still perfectly fine on an integration that delivers no header — which is
// exactly the shape the data stores take (db.corp.internal:5432, egress only).
func validateIntegrationHosts(hosts []string, hasHeader bool) error {
	if len(hosts) > maxIntegrationHosts {
		return fmt.Errorf("hosts: %d entries exceeds the %d-host limit", len(hosts), maxIntegrationHosts)
	}
	for i, h := range hosts {
		if err := proxy.ValidDomainEntry(h); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if hasHeader && !bareExactHost(h) {
			return fmt.Errorf("hosts[%d]: %q is a wildcard or carries a port, and a credential header is only ever added to a "+
				"bare exact host — the proxy cannot present the credential there. Name the host without a wildcard or port, "+
				"or clear the header", i, h)
		}
	}
	return nil
}

// bareExactHost reports whether h is a plain hostname the proxy's credential
// injector can actually match (Policy.AllowedExactHost) — no leading-"*."
// wildcard, no ":port" qualifier. Assumes h already passed
// proxy.ValidDomainEntry, so the only shapes left to exclude are those two.
func bareExactHost(h string) bool {
	h = strings.TrimSpace(h)
	return !strings.HasPrefix(h, "*.") && !strings.Contains(h, ":")
}

// validateIntegrationDelivery checks ONE secret row's delivery object.
//
// proxy_header's header is a trust boundary: it is written verbatim onto a
// forwarded request, so it must be a real HTTP field-name token
// (egress.ValidHeaderName — which excludes CR/LF, ':' and space by
// construction). Format is the other half of the same wire value: fmt.Sprintf
// substitutes the secret into it, so it needs exactly one %s (a format with
// none silently DROPS the credential and sends a bare prefix; one with two
// renders "%!s(MISSING)") and no CR/LF of its own; empty means the raw secret
// IS the value ("%s" — types.Integration.HeaderSecret materializes that).
// Fields belonging to a different mode are rejected rather than silently
// ignored — a row that LOOKS like it also names a path/var but doesn't use it
// is exactly the ambiguity this closed shape exists to prevent.
func validateIntegrationDelivery(d types.IntegrationDelivery) error {
	switch d.Mode {
	case types.DeliveryProxyHeader:
		if d.Path != "" || d.Var != "" {
			return fmt.Errorf("path/var: not part of proxy_header delivery")
		}
		if !egress.ValidHeaderName(d.Header) {
			return fmt.Errorf("header: %q is not a valid HTTP header name "+
				"(letters, digits and !#$%%&'*+-.^_`|~ only — no spaces, no ':', no line breaks)", d.Header)
		}
		if d.Format != "" {
			if strings.Count(d.Format, "%s") != 1 || strings.Count(d.Format, "%") != 1 {
				return fmt.Errorf("format: %q must contain exactly one %%s (where the secret goes) and no other verb", d.Format)
			}
			if strings.ContainsAny(d.Format, "\r\n") {
				return fmt.Errorf("format: must not contain a line break")
			}
		}
		return nil
	case types.DeliveryResidentFile:
		if d.Header != "" || d.Format != "" || d.Var != "" {
			return fmt.Errorf("header/format/var: not part of resident_file delivery")
		}
		if d.Path == "" || strings.ContainsAny(d.Path, "\r\n\x00") {
			return fmt.Errorf("path: resident_file delivery needs a sandbox file path")
		}
		return nil
	case types.DeliveryResidentEnv:
		if d.Header != "" || d.Format != "" || d.Path != "" {
			return fmt.Errorf("header/format/path: not part of resident_env delivery")
		}
		if d.Var == "" || strings.ContainsAny(d.Var, "=\r\n\x00 ") {
			return fmt.Errorf("var: resident_env delivery needs an environment variable name")
		}
		return nil
	default:
		return fmt.Errorf("mode: unknown %q (want proxy_header, resident_file or resident_env)", d.Mode)
	}
}

// hasProxyHeaderSecret reports whether any secret row delivers proxy_header —
// the condition that tightens the egress list to bare exact hosts
// (validateIntegrationHosts).
func hasProxyHeaderSecret(in types.Integration) bool {
	for _, s := range in.Secrets {
		if s.Delivery != nil && s.Delivery.Mode == types.DeliveryProxyHeader {
			return true
		}
	}
	return false
}

// knownIntegrationConfigKeys is each CLOSED kind's accepted non-secret config
// key set — an unknown key on a closed kind 400s BY NAME (a typo'd "regoin"
// silently stored would read back as an unset region at dispatch), while a
// GENERIC kind's config takes any string keys (the row is its own contract).
// github_app's "host" carries the legacy derivation's github.com marker
// (legacyIntegrations) so a derived row stays adoptable.
var knownIntegrationConfigKeys = map[string]map[string]bool{
	types.IntegrationKindBedrock:               {"region": true, "model": true, "auth_lane": true},
	types.IntegrationKindGitHubApp:             {"app_id": true, "installation_id": true, "host": true},
	types.IntegrationKindAnthropicSubscription: {"lane": true},
	types.IntegrationKindAnthropicAPIKey:       {},
	types.IntegrationKindOpenAIAPIKey:          {},
	types.IntegrationKindAzureOpenAI:           {},
	types.IntegrationKindGitHost:               {},
}

// validIntegrationDefaultFor is DefaultFor's closed value set (see
// types.Integration's doc comment: "agent_runs" and/or "wardyn_features").
var validIntegrationDefaultFor = map[string]bool{"agent_runs": true, "wardyn_features": true}

// validateIntegrationWrite enforces an operator-authored Integration's
// structural + security invariants before it is persisted (PUT
// /integrations/{id}, and defensively on adopt): id shape (integrationRefRE,
// workspaces.go — secretNameRE's charset plus the colon-qualified shape
// Wardyn's own legacy adoption mints and stores verbatim, e.g.
// "anthropic_subscription:managed", so an ADOPTED row stays editable through
// this same endpoint instead of being write-once); kind either in the closed
// set or a shape-valid generic slug; every secret row a real non-reserved
// secret name (validSecretRef — the same rule site-config's *SecretRef fields
// use) with a well-formed delivery (required on a generic kind — its row IS
// the contract; a closed kind may leave it nil, meaning the kind's own bespoke
// transport); config keys closed per kind (knownIntegrationConfigKeys — an
// unknown key on a closed kind 400s BY NAME; a generic kind takes any string
// keys); DefaultFor closed to {agent_runs, wardyn_features} on AI kinds only;
// and the two per-kind Config checks this codebase already has an established
// rule for: an artifact_mirror's ecosystems must be the same closed set
// ArtifactOverrides uses (site_config.go), and a bedrock integration may not
// half-override region/model — the identical hazard
// `git show ecc1903~1:internal/api/llmcred.go`'s
// TestValidateWorkspaceLLMCred_Rejections pinned for the pre-Integration
// shape (a region-scoped inference profile 403s at invoke with only one set).
// Config VALUES on a closed kind's known keys are left permissive beyond the
// bedrock pair: capabilitiesFor documents an unrecognized value as a graceful
// fallback, not an error, and this validator should not be stricter than the
// reader.
func validateIntegrationWrite(in types.Integration) error {
	if !integrationRefRE.MatchString(in.ID) {
		return fmt.Errorf("id: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars, "+
			"plus up to three colon-joined qualifier segments)", in.ID)
	}
	if !types.ClosedIntegrationKinds[in.Kind] && !secretNameRE.MatchString(in.Kind) {
		return fmt.Errorf("kind: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars)", in.Kind)
	}
	seenRole := make(map[string]bool, len(in.Secrets))
	for i, s := range in.Secrets {
		if !secretNameRE.MatchString(s.Role) {
			return fmt.Errorf("secrets[%d].role: invalid identifier %q", i, s.Role)
		}
		if seenRole[s.Role] {
			return fmt.Errorf("secrets[%d].role: duplicate %q", i, s.Role)
		}
		seenRole[s.Role] = true
		if s.SecretName == "" || !validSecretRef(s.SecretName) {
			return fmt.Errorf("secrets[%d].secret_name: invalid or reserved secret name %q", i, s.SecretName)
		}
		if s.Delivery == nil {
			if genericIntegrationKind(in.Kind) {
				return fmt.Errorf("secrets[%d].delivery: required — say how this secret reaches the run "+
					"(proxy_header, resident_file or resident_env); only a closed kind's bespoke transport may omit it", i)
			}
			continue
		}
		if err := validateIntegrationDelivery(*s.Delivery); err != nil {
			return fmt.Errorf("secrets[%d].delivery: %w", i, err)
		}
	}
	if err := validateIntegrationHosts(in.Egress, hasProxyHeaderSecret(in)); err != nil {
		return err
	}
	if in.Probe != nil {
		if in.Probe.Method != "GET" && in.Probe.Method != "HEAD" {
			return fmt.Errorf("probe.method: %q (want GET or HEAD)", in.Probe.Method)
		}
		if !validSiteURL(in.Probe.URL) {
			return fmt.Errorf("probe.url: invalid URL %q", in.Probe.URL)
		}
	}
	if len(in.Docs) > 2048 {
		return fmt.Errorf("docs: too long (%d bytes, max 2048)", len(in.Docs))
	}
	if len(in.DefaultFor) > 0 && !types.AIProviderKind(in.Kind) {
		// PLATFORM-API-3: both marks are defined only for the AI kinds
		// (types.Integration.DefaultFor's doc), and both readers
		// (applyDefaultForRadio's clear, defaultAgentRunsIntegration,
		// WardynFeaturesBackend) already filter on it — so a non-AI row taking
		// a mark here just STEALS it from the real AI row (applyDefaultForRadio
		// clears it from every OTHER row regardless of kind) while never being
		// able to SERVE it itself: silent, site-wide loss of model access
		// through a write that validated clean.
		return fmt.Errorf("default_for: only an AI provider integration may set this (kind is %q)", in.Kind)
	}
	for _, d := range in.DefaultFor {
		if !validIntegrationDefaultFor[d] {
			return fmt.Errorf("default_for: unknown %q (want agent_runs and/or wardyn_features)", d)
		}
	}
	if known, closed := knownIntegrationConfigKeys[in.Kind]; closed {
		for _, k := range slices.Sorted(maps.Keys(in.Config)) {
			if !known[k] {
				return fmt.Errorf("config: unknown key %q for kind %q", k, in.Kind)
			}
		}
	}
	if in.Kind == types.IntegrationKindBedrock {
		region, _ := in.Config["region"].(string)
		model, _ := in.Config["model"].(string)
		if (region == "") != (model == "") {
			return fmt.Errorf("config: bedrock region and model must be set together (a region-scoped inference profile 403s at invoke with only one)")
		}
	}
	if in.Kind == "artifact_mirror" {
		// track-b: B1 shim — keeps a derived topology row adoptable/editable
		// with the same closed-ecosystem rule it always had; dies in B2 with
		// the artifact_mirror derivation.
		for _, eco := range stringSlice(in.Config["ecosystems"]) {
			if !validArtifactEcosystems[eco] {
				return fmt.Errorf("config.ecosystems: unknown ecosystem %q", eco)
			}
		}
	}
	return nil
}

// applyDefaultForRadio enforces DefaultFor's RADIO semantics across rows: each
// value in newDefaultFor is a single-select mark, so setting it on the row
// named id CLEARS that same value from every OTHER row's DefaultFor in the
// same write — never a 409, per the approved spec. Mutates rows in place
// (mirroring applyDisabled's own in-place style above); a row with nothing to
// clear is left untouched (including its slice identity, so an unrelated
// write never appears to "touch" every other row).
func applyDefaultForRadio(rows []types.Integration, id string, newDefaultFor []string) {
	if len(newDefaultFor) == 0 {
		return
	}
	marks := make(map[string]bool, len(newDefaultFor))
	for _, m := range newDefaultFor {
		marks[m] = true
	}
	for i := range rows {
		if rows[i].ID == id || len(rows[i].DefaultFor) == 0 {
			continue
		}
		cleared := slices.DeleteFunc(slices.Clone(rows[i].DefaultFor), func(m string) bool { return marks[m] })
		if len(cleared) != len(rows[i].DefaultFor) {
			rows[i].DefaultFor = cleared
		}
	}
}

// resolveIntegrationRefFrom is resolveIntegrationRef's pure half: resolves
// ref against an ALREADY-COMPUTED effective set. Factored out (PLATFORM-API-8)
// so a caller resolving several refs in one request (applyWorkspaceRequirements,
// launchRecordRun) can compute effectiveIntegrations ONCE instead of once per
// ref — effectiveIntegrations reads the site-config store, a full secret
// listing, and peeks the subscription/Bedrock state, so recomputing it per ref
// multiplied that I/O by the requirement count. ok=false when ref is empty or
// names nothing at all.
func resolveIntegrationRefFrom(rows []integrationRow, ref string) (types.Integration, bool) {
	if ref == "" {
		return types.Integration{}, false
	}
	for _, row := range rows {
		if row.ID == ref {
			return row.Integration, true
		}
	}
	return types.Integration{}, false
}

// resolveIntegrationRef resolves ref against the EFFECTIVE integration set
// (stored ∪ legacy-derived — effectiveIntegrations above) into the concrete
// types.Integration it names. Effective, not stored-only, so a run/workspace
// binding "just works" against a well-known legacy id (e.g.
// "anthropic_api_key") with no adoption step required first — the entire
// point of deriving legacy rows in the first place. ok=false when ref is
// empty or names nothing at all. The single-ref convenience form — a caller
// resolving MULTIPLE refs in one request should compute effectiveIntegrations
// once and call resolveIntegrationRefFrom directly (PLATFORM-API-8).
func (s *Server) resolveIntegrationRef(ctx context.Context, ref string) (types.Integration, bool) {
	if ref == "" {
		return types.Integration{}, false
	}
	present := s.presentSecretNames(ctx)
	return resolveIntegrationRefFrom(s.effectiveIntegrations(ctx, present, s.setupBedrock(ctx, present)), ref)
}

// defaultAgentRunsIntegration returns the STORED AI-provider integration
// marked DefaultFor: agent_runs, optionally narrowed to onlyType (""=any
// type). Only a STORED row can carry DefaultFor at all (a legacy-derived row
// is never persisted, so it never has one — see types.Integration's doc
// comment), which is exactly what keeps this tier a no-op with zero stored
// integrations regardless of onlyType. ok=false when none is marked.
func (s *Server) defaultAgentRunsIntegration(ctx context.Context, onlyType string) (types.Integration, bool) {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		if got, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			sc = got
		}
	}
	for _, in := range sc.Integrations {
		if !types.AIProviderKind(in.Kind) || !slices.Contains(in.DefaultFor, "agent_runs") {
			continue
		}
		if onlyType != "" && in.Kind != onlyType {
			continue
		}
		return in, true
	}
	return types.Integration{}, false
}

// WardynFeaturesBackend returns the STORED AI-provider integration marked
// DefaultFor: wardyn_features whose wardyn_features capability reads
// "available" right now (capabilitiesFor above — never a needs_setup/off/
// impossible one), for cmd/wardynd's composer-registry boot derivation
// (WARDYN_COMPOSER_CONFIG unset — see cmd/wardynd/composer.go). Exported as a
// plain function of an already-fetched SiteConfig plus the same live signals
// liveCapEnv folds from Server config, because cmd/wardynd builds the
// composer registry BEFORE the api.Server exists (Config.Composer is
// late-bound INTO it once built) — there is no live Server here to read them
// from. ok=false (no eligible integration) is the signal to keep today's
// behavior: no registry, compose 404s honestly.
func WardynFeaturesBackend(sc types.SiteConfig, secretPresent func(string) bool, bedrockRegionSet, bedrockModelSet bool, managedBlobPresent func(string) bool) (types.Integration, bool) {
	env := capEnv{
		SecretPresent: secretPresent, BedrockRegionSet: bedrockRegionSet,
		BedrockModelSet: bedrockModelSet, ManagedBlobPresent: managedBlobPresent,
	}
	for _, in := range sc.Integrations {
		if !types.AIProviderKind(in.Kind) || !slices.Contains(in.DefaultFor, "wardyn_features") {
			continue
		}
		for _, c := range capabilitiesFor(toIntegrationView(integrationRow{Integration: in}), env) {
			if c.ID == "wardyn_features" && c.State == CapAvailable {
				return in, true
			}
		}
	}
	return types.Integration{}, false
}
