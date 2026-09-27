// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Entra lane's half of the workspace-provider write boundary: the rules a
// row carrying `lanes: [entra]`, an `entra` block or `credential_source:
// per_user` is held to.
//
// It is a SEPARATE FILE from workspace_providers.go for one reason —
// scripts/check-file-size.sh — and not because the rules are separable: they
// run inside validateWorkspaceProviders like every other row rule, on both
// doors, in one pass.
package api

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PROVIDERS_400_ENTRA — the refusal bodies this file writes. DRAFT, on the
// same M2 canon footing as the constants in workspace_providers.go, and
// asserted through the constants for the same reason.
const (
	providers400EntraHost = "git[%d].entra: the entra lane is available on dev.azure.com and <org>.visualstudio.com only — %q does not accept Entra tokens"
	providers400EntraNone = "git[%d].entra: the entra lane needs an entra block naming the tenant and the client"
	providers400EntraOrph = "git[%d].entra: an entra block means nothing on a row whose lanes do not include %q"
	providers400EntraGUID = "git[%d].entra.%s: %q is not a GUID — a tenant and a client are named by GUID, never by an alias"
	providers400EntraCap  = "git[%d].entra.%s[%d]: %q is not a capability — want one of: %s"
	providers400EntraCeil = "git[%d].entra.capability_ceiling: name at least one capability — an empty ceiling has no reading that is not a guess"
	providers400EntraRead = "git[%d].entra.capability_ceiling: name %q — every profile starts from reads, so a ceiling without it can serve nothing"
	providers400EntraShar = "git[%d].credential_source: the %q lane needs per_user — there is no such thing as a shared Entra sign-in"
	providers400EntraProf = "git[%d].entra.default_profile: %q is outside capability_ceiling"
	providers400EntraMint = "git[%d].entra.token_mode: minted_pat is not available — Azure DevOps only lets Microsoft's own clients mint personal access tokens, so Wardyn cannot mint one per run; use bearer"
	providers400EntraMode = "git[%d].entra.token_mode: %q is not a token mode — want one of: %s"
	providers400Source    = "git[%d].credential_source: %q is not a credential source — want one of: %s"
	providers400PerUser   = "git[%d].credential_source: per_user needs the %q lane — no other git lane authorizes a person as themselves"
	providers400EntraTwo  = "git[%d].lanes: git[%d] already carries the %q lane — a person signs in to one Azure DevOps organisation per deployment, so disable one of the two rows"
)

// entraGUID is the tenant/client id shape.
//
// A GUID and NOTHING ELSE. Entra also accepts a verified domain and the
// aliases "common" and "organizations" wherever a tenant is named, and every
// one of those is refused here: "common" means "whichever tenant the person
// happens to belong to", which is the exact opposite of an organisation
// pinning its own, and a domain is a name that can be re-pointed.
var entraGUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateProviderEntra holds one row's Entra lane, Entra block and credential
// source to their invariants.
//
// It runs for EVERY row, not only the ones with a lane, because two of the
// rules are about a row that did NOT opt in: an entra block without the lane
// reads as configuration that does nothing, and per_user without the lane
// reads as per-person authorization on a lane that has no per-person path.
// Both are an admin's intent silently not taking effect, which is the failure
// this whole block exists to stop.
func validateProviderEntra(i int, row types.GitProvider) error {
	hasLane := slices.Contains(row.Lanes, types.GitLaneEntra)
	if src := row.CredentialSource; src != "" {
		if !src.Valid() {
			return fmt.Errorf(providers400Source, i, string(src),
				strings.Join(types.ClosedCredentialSourceList(), ", "))
		}
		if src == types.CredentialSourcePerUser && !hasLane {
			return fmt.Errorf(providers400PerUser, i, string(types.GitLaneEntra))
		}
	}
	if !hasLane {
		if row.Entra != nil {
			return fmt.Errorf(providers400EntraOrph, i, string(types.GitLaneEntra))
		}
		return nil
	}
	if err := entraLaneHosts(i, row); err != nil {
		return err
	}
	// The lane REQUIRES per_user, rather than merely permitting it. An Entra
	// sign-in is a person authenticating as themselves against their own
	// tenant; "shared" would have to mean one person's identity silently
	// backing everyone else's runs, which is the failure this lane exists to
	// end. Refusing it here beats storing a row whose credential_source has no
	// defined meaning.
	if row.CredentialSource != types.CredentialSourcePerUser {
		return fmt.Errorf(providers400EntraShar, i, string(types.GitLaneEntra))
	}
	if row.Entra == nil {
		return fmt.Errorf(providers400EntraNone, i)
	}
	return validateEntraBlock(i, *row.Entra)
}

// validateOneEntraRow refuses a second ENABLED row on the entra lane. The
// sign-in is served for one row only (cmd/wardynd's adoEntraRow takes the
// first), so a second row would store as valid, never be offered a sign-in,
// and have every run on it refused later for a cause that is not the real
// one. A disabled row is not served and so is not counted. It runs after the
// per-row rules, so every row counted here is a valid per_user Entra row.
func validateOneEntraRow(rows []types.GitProvider) error {
	first := -1
	for i, row := range rows {
		if row.Disabled || !slices.Contains(row.Lanes, types.GitLaneEntra) {
			continue
		}
		if first >= 0 {
			return fmt.Errorf(providers400EntraTwo, i, first, string(types.GitLaneEntra))
		}
		first = i
	}
	return nil
}

// entraLaneHosts refuses the lane on a host that cannot carry it. Azure DevOps
// SERVER does not accept an Entra access token at all, and a self-hosted host
// is indistinguishable from an ADO Server host by name, so the rule is stated
// the safe way round: the lane is permitted on the two HOSTED addresses and
// nowhere else. The kind follows from that — github.com and every ADO host
// belong to exactly one kind each at this point in validation.
func entraLaneHosts(i int, row types.GitProvider) error {
	for _, raw := range row.BaseURLs {
		host := strings.ToLower(hostrules.HostOf(raw))
		if host != "dev.azure.com" && !strings.HasSuffix(host, ".visualstudio.com") {
			return fmt.Errorf(providers400EntraHost, i, host)
		}
	}
	return nil
}

// validateEntraBlock holds the block's own fields: two GUIDs, a non-empty
// ceiling of grantable capabilities, a default profile inside that ceiling,
// and a known token mode.
func validateEntraBlock(i int, cfg types.ADOEntraConfig) error {
	if !entraGUID.MatchString(cfg.TenantID) {
		return fmt.Errorf(providers400EntraGUID, i, "tenant_id", cfg.TenantID)
	}
	if !entraGUID.MatchString(cfg.ClientID) {
		return fmt.Errorf(providers400EntraGUID, i, "client_id", cfg.ClientID)
	}
	if len(cfg.CapabilityCeiling) == 0 {
		return fmt.Errorf(providers400EntraCeil, i)
	}
	if err := grantableCapabilities(i, "capability_ceiling", cfg.CapabilityCeiling); err != nil {
		return err
	}
	// The ceiling must admit READS. Every profile starts from them — the
	// default one IS them — so a ceiling without read describes a lane that
	// would refuse the first request a run makes, and the contradiction is
	// cheaper to catch at the write than to debug at the forge.
	if !slices.Contains(cfg.CapabilityCeiling, adoscope.CapRead) {
		return fmt.Errorf(providers400EntraRead, i, string(adoscope.CapRead))
	}
	if err := grantableCapabilities(i, "default_profile", cfg.DefaultProfile); err != nil {
		return err
	}
	// cfg.Profile() and not cfg.DefaultProfile: an empty profile reads as the
	// catalogue's read-only one, so the ceiling has to admit THAT — a ceiling
	// without `read` and a row that named no profile would otherwise store as
	// valid and refuse every request the row exists to serve.
	for _, c := range cfg.Profile() {
		if !slices.Contains(cfg.CapabilityCeiling, c) {
			return fmt.Errorf(providers400EntraProf, i, string(c))
		}
	}
	// minted_pat is refused BY NAME, ahead of the generic closed-set check, so
	// the admin reads why rather than "not a token mode".
	if cfg.TokenMode == types.ADOTokenModeMintedPAT {
		return fmt.Errorf(providers400EntraMint, i)
	}
	if cfg.TokenMode != "" && !cfg.TokenMode.Valid() {
		return fmt.Errorf(providers400EntraMode, i, string(cfg.TokenMode),
			strings.Join(types.ClosedADOTokenModeList(), ", "))
	}
	return nil
}

// grantableCapabilities refuses a capability a row may not name: an unknown
// string, but also a DENIED area or the unclassified-write placeholder, which
// are classifier answers rather than access anyone can be granted.
func grantableCapabilities(i int, field string, caps []adoscope.Capability) error {
	for j, c := range caps {
		if !c.Grantable() {
			return fmt.Errorf(providers400EntraCap, i, field, j, string(c),
				adoscope.GrantableCapabilityList())
		}
	}
	return nil
}
