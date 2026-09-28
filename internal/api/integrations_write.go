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

// integrations_write.go is the write + resolution half of integrations.go,
// split out (scripts/check-file-size.sh) once the file grew past the 1000-line
// gate: validateIntegrationWrite backs the write endpoints
// (setup_integrations.go); resolveIntegrationRef resolves a ref against the
// effective set. Everything here builds ON TOP of
// effectiveIntegrations/capabilitiesFor (integrations.go) without changing
// either.

// genericIntegrationKind reports whether kind is a GENERIC open slug — one
// whose behavior does NOT come from code: the row's own secrets, egress and
// delivery are the whole contract (see types.ClosedIntegrationKinds). No slug
// is excepted: "artifact_mirror"/"host_proxy" as a NEW-shape kind are ordinary
// generic connections (the legacy topology CATEGORIES are dropped at the fold,
// types.IntegrationList, and no longer derived — integrations.go).
func genericIntegrationKind(kind string) bool {
	return !types.ClosedIntegrationKinds[kind]
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

// residentDeliveryRefusal is why an operator-authored secret row may not
// declare a resident delivery today. types.Integration models three modes
// because the two resident ones DO happen — but only on a closed kind's
// hand-written transport (git_host's ssh_key file, bedrock's AWS env), which
// is exactly the case that leaves Delivery nil. There is no generic lane that
// materializes an arbitrary named secret into an arbitrary sandbox path or env
// var, so accepting one here would store a promise nothing keeps: the row
// would read "delivered" on the surface and deliver nothing to the run.
const residentDeliveryRefusal = "delivery.mode %q is not deliverable for an operator-authored integration: Wardyn " +
	"has no generic lane that materializes a named secret into the sandbox (the resident lanes that exist are " +
	"hand-written per provider — git_host's SSH key, Bedrock's AWS env — and those closed kinds declare no delivery " +
	"at all). Use proxy_header, or leave the delivery off on a closed kind whose own transport carries it"

// validateIntegrationDelivery checks ONE secret row's delivery object.
//
// proxy_header is the only mode an operator may declare (see
// residentDeliveryRefusal). Its header is a trust boundary: it is written
// verbatim onto a forwarded request, so it must be a real HTTP field-name token
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
		// ONE implementation of the format rule, shared with the api_key
		// eligible-grant path (validInjectionFormat, policy.go) — the two
		// authoring paths write the identical wire field.
		if err := validInjectionFormat(d.Format); err != nil {
			return err
		}
		return nil
	case types.DeliveryResidentFile, types.DeliveryResidentEnv:
		return fmt.Errorf(residentDeliveryRefusal, d.Mode)
	default:
		return fmt.Errorf("mode: unknown %q (want proxy_header, resident_file or resident_env)", d.Mode)
	}
}

// hasProxyHeaderSecret reports whether any secret row delivers proxy_header —
// the condition that tightens the egress list to bare exact hosts
// (validateIntegrationHosts).
func hasProxyHeaderSecret(in types.Integration) bool {
	return countProxyHeaderSecrets(in) > 0
}

// countProxyHeaderSecrets counts the row's proxy_header-delivered secrets.
// More than one is refused at write time (validateIntegrationWrite): the
// proxy's injector is keyed BY HOST (buildInjector's byHost map, one rule per
// host) and every proxy_header secret of a row would target that row's whole
// egress list — so a second one has nowhere of its own to go and would be
// silently dropped at dispatch.
func countProxyHeaderSecrets(in types.Integration) int {
	n := 0
	for _, s := range in.Secrets {
		if s.Delivery != nil && s.Delivery.Mode == types.DeliveryProxyHeader {
			n++
		}
	}
	return n
}

// knownIntegrationConfigKeys is each CLOSED kind's accepted non-secret config
// key set — an unknown key on a closed kind 400s BY NAME (a typo'd
// "instalation_id" silently stored would read back as unset), while a
// GENERIC kind's config takes any string keys (the row is its own contract).
// github_app's "host" carries the legacy derivation's github.com marker
// (legacyIntegrations, integrations.go) so a derived row stays adoptable.
var knownIntegrationConfigKeys = map[string]map[string]bool{
	types.IntegrationKindGitHubApp: {"app_id": true, "installation_id": true, "host": true},
	types.IntegrationKindGitHost:   {},
}

// integration400AIKind refuses an AI-kind write: model access is a model
// provider now, and effectiveIntegrations leaves a stored AI row out of every
// resolver, so a row written here would grant nothing.
const integration400AIKind = "kind: %q is model access, which is set up under Settings → Model providers — an integration no longer carries it"

// validateIntegrationWrite enforces an operator-authored Integration's
// structural + security invariants before it is persisted (PUT
// /integrations/{id}, and defensively on adopt): id shape (integrationRefRE,
// workspace_requirements.go — secretNameRE's charset plus the colon-qualified
// shape Wardyn's own legacy adoption mints and stores verbatim, e.g.
// "anthropic_subscription:managed", so an ADOPTED row stays editable through
// this same endpoint instead of being write-once); kind either in the closed
// set or a shape-valid generic slug; every secret row a real non-reserved
// secret name (validSecretRef — the same rule site-config's *SecretRef fields
// use) with a well-formed delivery (required on a generic kind — its row IS
// the contract; a closed kind may leave it nil, meaning the kind's own bespoke
// transport); config keys closed per kind (knownIntegrationConfigKeys — an
// unknown key on a closed kind 400s BY NAME; a generic kind takes any string
// keys). Config VALUES on a closed kind's known keys are left permissive:
// capabilitiesFor documents an unrecognized value as a graceful fallback, not
// an error, and this validator should not be stricter than the reader.
func validateIntegrationWrite(in types.Integration) error {
	if !integrationRefRE.MatchString(in.ID) {
		return fmt.Errorf("id: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars, "+
			"plus up to three colon-joined qualifier segments)", in.ID)
	}
	// Only the CLOSED kinds are writable. Generic kinds (package feeds,
	// container registries, cloud providers, data stores, MCP servers, work
	// tracking, observability, "other service") were an operator-extensibility
	// surface for the /integrations catalog, and that catalog is gone —
	// connections are the four Settings cards now, over closed kinds only.
	//
	// A row stored under an earlier release still DESERIALIZES (the read-time
	// fold in internal/types is a passthrough, so nothing an operator configured
	// disappears from site config or stops being injected by
	// integrations_run.go). It simply can no longer be edited through this
	// endpoint. This is a deliberate capability removal, recorded under BREAKING
	// in the changelog — not a validation tightening that fell out of a refactor.
	if types.AIProviderKind(in.Kind) {
		return fmt.Errorf(integration400AIKind, in.Kind)
	}
	if !types.ClosedIntegrationKinds[in.Kind] {
		return fmt.Errorf("kind: %q is not a supported integration kind (want one of: %s)",
			in.Kind, strings.Join(types.ClosedIntegrationKindList(), ", "))
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
		// A closed kind may omit delivery — its bespoke transport supplies it.
		// The generic branch here (delivery REQUIRED, because nothing else could
		// say how the secret reaches the run) is unreachable now that the kind
		// gate above refuses every non-closed kind.
		if s.Delivery == nil {
			continue
		}
		if err := validateIntegrationDelivery(*s.Delivery); err != nil {
			return fmt.Errorf("secrets[%d].delivery: %w", i, err)
		}
	}
	if countProxyHeaderSecrets(in) > 1 {
		return fmt.Errorf("secrets: at most one proxy_header-delivered secret per integration — the proxy injects " +
			"ONE credential header per host and every such secret targets this row's whole egress list, so a second " +
			"one has no host of its own and would be silently dropped at dispatch. Split the second credential into " +
			"its own integration")
	}
	if err := validateIntegrationHosts(in.Egress, hasProxyHeaderSecret(in)); err != nil {
		return err
	}
	if len(in.Docs) > 2048 {
		return fmt.Errorf("docs: too long (%d bytes, max 2048)", len(in.Docs))
	}
	if known, closed := knownIntegrationConfigKeys[in.Kind]; closed {
		for _, k := range slices.Sorted(maps.Keys(in.Config)) {
			if !known[k] {
				return fmt.Errorf("config: unknown key %q for kind %q", k, in.Kind)
			}
		}
	}
	return nil
}

// resolveIntegrationRefFrom is resolveIntegrationRef's pure half: resolves
// ref against an ALREADY-COMPUTED effective set. Factored out so a caller
// resolving several refs in one request (applyWorkspaceRequirements,
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
// types.Integration it names. Effective, not stored-only, so a workspace
// requirement "just works" against a well-known legacy id (e.g.
// "git_host:github.com") with no adoption step required first — the entire
// point of deriving legacy rows in the first place. ok=false when ref is
// empty or names nothing at all. The single-ref convenience form — a caller
// resolving MULTIPLE refs in one request should compute effectiveIntegrations
// once and call resolveIntegrationRefFrom directly.
// owner (secretOwnerFromRequest — "" for an operator) widens the presence map
// this resolves against to include the caller's OWN stored secrets. "" is the
// operator namespace.
func (s *Server) resolveIntegrationRef(ctx context.Context, owner, ref string) (types.Integration, bool) {
	if ref == "" {
		return types.Integration{}, false
	}
	return resolveIntegrationRefFrom(s.effectiveIntegrations(ctx, s.presentSecretNamesFor(ctx, owner)), ref)
}
