// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
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
// back the run-time resolution ladder (llmcred.go); namedIntegrationTypes
// backs gen.go's agent-tool-bake derivation (workspace_run.go/workspaces.go);
// WardynFeaturesBackend backs the composer-registry boot derivation
// (cmd/wardynd/composer.go). Everything here builds ON TOP of
// effectiveIntegrations/capabilitiesFor (integrations.go) without changing
// either.

// knownIntegrationTypes is the closed type set PER CATEGORY a write may name —
// a hand-kept mirror of capabilitiesFor's switch above (frozen; see this
// file's own doc comment) rather than a reflection-based derivation, so a type
// this list is missing fails LOUD here ("invalid type") instead of silently
// passing validation and landing on "no capabilities" in the live matrix.
var knownIntegrationTypes = map[types.IntegrationCategory]map[string]bool{
	types.IntegrationAIProvider: {
		"anthropic_api_key": true, "anthropic_subscription": true, "bedrock": true,
		"openai_api_key": true, "azure_openai": true,
	},
	types.IntegrationSCMHost:        {"github_app": true, "git_host": true},
	types.IntegrationArtifactMirror: {"artifact_mirror": true},
	types.IntegrationHostProxy:      {"host_proxy": true},
}

// genericIntegrationCategories are the categories whose behavior does NOT
// depend on Type (see types.IntegrationCategory): the row's own hosts, header
// and secret ref are the whole contract, so Type is an open slug validated for
// shape only. Enumerating ~35 provider types here would buy nothing but a
// second hand-kept mirror of the UI catalog to drift against.
var genericIntegrationCategories = map[types.IntegrationCategory]bool{
	types.IntegrationPackageFeed:       true,
	types.IntegrationContainerRegistry: true,
	types.IntegrationCloudProvider:     true,
	types.IntegrationDataStore:         true,
	types.IntegrationMCPServer:         true,
	types.IntegrationWorkTracking:      true,
	types.IntegrationObservability:     true,
	types.IntegrationOtherService:      true,
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

// validateIntegrationCredentialDelivery checks the proxy-injected delivery
// triple (Header, Format, and the secret the header carries).
//
// Header is a trust boundary: it is written verbatim onto a forwarded request,
// so it must be a real HTTP field-name token (egress.ValidHeaderName — which
// excludes CR/LF, ':' and space by construction). Format is the other half of
// the same wire value: fmt.Sprintf substitutes the secret into it, so it needs
// exactly one %s (a format with none silently DROPS the credential and sends a
// bare prefix; one with two renders "%!s(MISSING)") and no CR/LF of its own.
func validateIntegrationCredentialDelivery(in types.Integration) error {
	if in.Header == "" {
		if in.Format != "" {
			return fmt.Errorf("format: set without a header — nothing presents this value")
		}
		return nil
	}
	if !egress.ValidHeaderName(in.Header) {
		return fmt.Errorf("header: %q is not a valid HTTP header name "+
			"(letters, digits and !#$%%&'*+-.^_`|~ only — no spaces, no ':', no line breaks)", in.Header)
	}
	if in.Format != "" {
		if strings.Count(in.Format, "%s") != 1 || strings.Count(in.Format, "%") != 1 {
			return fmt.Errorf("format: %q must contain exactly one %%s (where the secret goes) and no other verb", in.Format)
		}
		if strings.ContainsAny(in.Format, "\r\n") {
			return fmt.Errorf("format: must not contain a line break")
		}
	}
	if in.Credentials[types.IntegrationCredentialToken] == "" {
		return fmt.Errorf("credentials[%s]: a header is set but names no secret to present in it", types.IntegrationCredentialToken)
	}
	return nil
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
// this same endpoint instead of being write-once), category/type against the
// known sets above, every credential value a real non-reserved secret name
// (validSecretRef — the same rule site-config's *SecretRef fields use),
// DefaultFor closed to {agent_runs, wardyn_features}, and the two per-type
// Config checks this codebase already has an established rule for: an
// artifact_mirror's ecosystems must be the same closed set ArtifactOverrides
// uses (site_config.go), and a bedrock integration may not half-override
// region/model — the identical hazard
// `git show ecc1903~1:internal/api/llmcred.go`'s
// TestValidateWorkspaceLLMCred_Rejections pinned for the pre-Integration
// shape (a region-scoped inference profile 403s at invoke with only one set).
// Other per-type Config knobs (e.g. a subscription's lane) are deliberately
// left permissive: capabilitiesFor documents an unrecognized value as a
// graceful fallback, not an error, and this validator should not be stricter
// than the reader.
func validateIntegrationWrite(in types.Integration) error {
	if !integrationRefRE.MatchString(in.ID) {
		return fmt.Errorf("id: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars, "+
			"plus up to three colon-joined qualifier segments)", in.ID)
	}
	switch knownTypes, typed := knownIntegrationTypes[in.Category]; {
	case typed:
		if !knownTypes[in.Type] {
			return fmt.Errorf("type: %q is not a known %s type", in.Type, in.Category)
		}
	case genericIntegrationCategories[in.Category]:
		// Open type set — shape only (see genericIntegrationCategories).
		if !secretNameRE.MatchString(in.Type) {
			return fmt.Errorf("type: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars)", in.Type)
		}
	default:
		return fmt.Errorf("category: unknown %q", in.Category)
	}
	for role, ref := range in.Credentials {
		if ref != "" && !validSecretRef(ref) {
			return fmt.Errorf("credentials[%s]: invalid or reserved secret name %q", role, ref)
		}
	}
	if err := validateIntegrationHosts(in.Hosts, in.Header != ""); err != nil {
		return err
	}
	if err := validateIntegrationCredentialDelivery(in); err != nil {
		return err
	}
	if len(in.Docs) > 2048 {
		return fmt.Errorf("docs: too long (%d bytes, max 2048)", len(in.Docs))
	}
	for _, d := range in.DefaultFor {
		if !validIntegrationDefaultFor[d] {
			return fmt.Errorf("default_for: unknown %q (want agent_runs and/or wardyn_features)", d)
		}
	}
	if len(in.Config) == 0 {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(in.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if in.Type == "bedrock" {
		region, _ := cfg["region"].(string)
		model, _ := cfg["model"].(string)
		if (region == "") != (model == "") {
			return fmt.Errorf("config: bedrock region and model must be set together (a region-scoped inference profile 403s at invoke with only one)")
		}
	}
	if in.Category == types.IntegrationArtifactMirror {
		for _, eco := range stringSlice(cfg["ecosystems"]) {
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

// resolveIntegrationRef resolves ref against the EFFECTIVE integration set
// (stored ∪ legacy-derived — effectiveIntegrations above) into the concrete
// types.Integration it names. Effective, not stored-only, so a run/workspace
// binding "just works" against a well-known legacy id (e.g.
// "anthropic_api_key") with no adoption step required first — the entire
// point of deriving legacy rows in the first place. ok=false when ref is
// empty or names nothing at all.
func (s *Server) resolveIntegrationRef(ctx context.Context, ref string) (types.Integration, bool) {
	if ref == "" {
		return types.Integration{}, false
	}
	for _, row := range s.effectiveIntegrations(ctx) {
		if row.ID == ref {
			return row.Integration, true
		}
	}
	return types.Integration{}, false
}

// namedIntegrationTypes returns the .Type of every integration ws's
// EFFECTIVE requirements contract names via an "integration:<id>" key, at ANY
// level (required or optional) — naming one, even optionally, is already a
// deliberate operator act (validateWorkspaceRequirement's own comment on the
// "integration" requirement kind makes the same call: no existence gate
// either, since a contract may name an integration before it's configured).
// Feeds workspacescan.AgentToolsForIntegrationTypes so the recommended build
// bakes the matching agent CLI (gen.go). Best-effort: a ref that resolves to
// nothing (named before the integration exists) is skipped, not an error.
func (s *Server) namedIntegrationTypes(ctx context.Context, ws types.Workspace) []string {
	var out []string
	for key := range effectiveRequirements(ws) {
		typ, rest, ok := splitRequirementKey(key)
		if !ok || typ != "integration" {
			continue
		}
		if in, ok := s.resolveIntegrationRef(ctx, rest); ok {
			out = append(out, in.Type)
		}
	}
	return out
}

// defaultAgentRunsIntegration returns the STORED ai_provider integration
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
		if in.Category != types.IntegrationAIProvider || !slices.Contains(in.DefaultFor, "agent_runs") {
			continue
		}
		if onlyType != "" && in.Type != onlyType {
			continue
		}
		return in, true
	}
	return types.Integration{}, false
}

// WardynFeaturesBackend returns the STORED ai_provider integration marked
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
		if in.Category != types.IntegrationAIProvider || !slices.Contains(in.DefaultFor, "wardyn_features") {
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
