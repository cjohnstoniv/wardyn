// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Model providers: the admin-written records that say which kinds of model
// credential this deployment supports, where each one sends requests and which
// harnesses may use it. This file owns the write boundary every door runs —
// normalization, validation and the server-owned UID. Configuration ONLY: no
// credential is ever read or written here, and nothing at run create or
// dispatch reads these records yet, so a nil block changes nothing.
package api

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mp400* — the refusal bodies this file writes. DRAFT (M2 canon pending), held
// as constants for the reason agent_providers.go gives for its own.
const (
	mp400ID           = "model_providers[%d].id: %q is not a provider id — lowercase letters, digits and ._- , at most 64 characters"
	mp400DupID        = "model_providers: id %q is not unique"
	mp400Kind         = "model_providers[%d].kind: %q is not a model-provider kind — want one of: %s"
	mp400Name         = "model_providers: %q: name must be at most %d characters, with no control characters"
	mp400BaseURLNeed  = "model_providers: %q: base_url is required for a custom_endpoint provider — it is where every request goes"
	mp400BaseURLBR    = "model_providers: %q: a Bedrock provider's address is bedrock.base_url, not base_url"
	mp400BaseURL      = "model_providers: %q: base_url: %w"
	mp400AuthUnused   = "model_providers: %q: auth applies only to a custom_endpoint provider — every other kind sends each person's credential the vendor's own way"
	mp400Header       = "model_providers: %q: %s: %q is not a valid HTTP header name"
	mp400Format       = "model_providers: %q: %s: %w"
	mp400BRNeed       = "model_providers: %q: a %s provider needs bedrock.region"
	mp400BRUnused     = "model_providers: %q: bedrock applies only to the bedrock_sso and bedrock_bearer kinds"
	mp400Region       = "model_providers: %q: bedrock.region %q is not an AWS region"
	mp400BRBaseURL    = "model_providers: %q: bedrock.base_url: %w"
	mp400StartURLNeed = "model_providers: %q: bedrock.sso_start_url is required for a bedrock_sso provider — it is what makes each person's sign-in one click"
	mp400StartURL     = "model_providers: %q: bedrock.sso_start_url: %w"
	mp400SSOUnused    = "model_providers: %q: bedrock.sso_start_url, sso_account_id and sso_role_name apply only to a bedrock_sso provider"
	mp400PinPair      = "model_providers: %q: bedrock.sso_account_id and sso_role_name are set together or not at all — pinning the account alone still leaves the role picked for whoever signs in"
	mp400AccountID    = "model_providers: %q: bedrock.sso_account_id must be a 12-digit AWS account id"
	mp400RoleName     = "model_providers: %q: bedrock.sso_role_name must be an IAM role name — letters, digits and +=,.@_- , at most 64 characters"
	mp400Incompatible = "model_providers: %q: %s"
	mp400Harness      = "model_providers: %q: %q is not an agent Wardyn can wire a model provider for — want one of: %s"
	mp400HarnessDup   = "model_providers: %q: %q is listed twice under harnesses"
	mp400ModelNeed    = "model_providers: %q: %s: model is required on a Bedrock provider — an inference profile id"
	mp400Model        = "model_providers: %q: %s: model %q is not a model id — letters, digits and ._:/@+[]- , at most 256 characters"
	mp400EndpointOnly = "model_providers: %q: %s: path, auth_header and auth_format apply only to a custom_endpoint provider"
	mp400Path         = "model_providers: %q: %s: path %q must start with a single / and carry no query, fragment, backslash, whitespace or .. segment"
)

// Name is the one free-text field on a record a member reads; bounded so a
// picker row stays a row.
const maxModelProviderName = 100

// Default header scheme of a custom endpoint when the admin names none — the
// editor's own defaults, written onto the record so a later explicit
// "Authorization" is not mistaken for an address or scheme change.
const (
	defaultEndpointAuthHeader = "Authorization"
	defaultEndpointAuthFormat = "Bearer %s"
)

// modelProviderIDPattern is a provider id's grammar. No ":" (which an
// integration id may carry): the audit writes a provider id into
// "<harness>:<id>" and "<id>:<account>/<role>", which a colon would make
// ambiguous.
var modelProviderIDPattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// modelIDPattern admits every model id shape the two harnesses take: vendor ids
// ("claude-sonnet-4-5[1m]", "gpt-5-codex"), Bedrock inference-profile ids and
// ARNs (":" and "/"). It excludes quotes, whitespace and control characters
// because the value is written into an environment variable and into Codex's
// config.toml at prep.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+\[\]-]{1,256}$`)

// providerPublicHosts is the vendor host each kind's base URL re-points — rule
// 5's "must not equal the public provider host". A custom endpoint serves both
// dialects, so it is held against both.
func providerPublicHosts(k types.ModelProviderKind) []string {
	switch k {
	case types.ModelProviderAnthropicSubscription, types.ModelProviderAnthropicAPIKey:
		return []string{"api.anthropic.com"}
	case types.ModelProviderOpenAIAPIKey:
		return []string{"api.openai.com"}
	case types.ModelProviderCustomEndpoint:
		return []string{"api.anthropic.com", "api.openai.com"}
	}
	return nil
}

// modelProviderRows is the stored providers, or nil.
func modelProviderRows(sc types.SiteConfig) []types.ModelProvider {
	if sc.ModelProviders == nil {
		return nil
	}
	return sc.ModelProviders.Providers
}

// enabledModelProviderCount is site_config.write's count of providers that are
// not turned off.
func enabledModelProviderCount(sc types.SiteConfig) int {
	n := 0
	for _, p := range modelProviderRows(sc) {
		if !p.Disabled {
			n++
		}
	}
	return n
}

// normalizeModelProviders canonicalizes a block on its way to storage and
// returns nil for an EMPTY one, which is what makes {} the clear form. Only
// whitespace and a trailing "/" are trimmed — ids and kinds have exact grammars
// and an off-case value is refused, never rewritten — and a custom endpoint with
// no header scheme is given the default one.
func normalizeModelProviders(p *types.ModelProviders) *types.ModelProviders {
	if p == nil {
		return nil
	}
	for i := range p.Providers {
		mp := &p.Providers[i]
		mp.ID, mp.Name = strings.TrimSpace(mp.ID), strings.TrimSpace(mp.Name)
		mp.BaseURL = strings.TrimSuffix(strings.TrimSpace(mp.BaseURL), "/")
		if mp.Bedrock != nil {
			b := mp.Bedrock
			b.Region, b.SSOStartURL = strings.TrimSpace(b.Region), strings.TrimSpace(b.SSOStartURL)
			b.BaseURL = strings.TrimSuffix(strings.TrimSpace(b.BaseURL), "/")
			b.SSOAccountID, b.SSORoleName = strings.TrimSpace(b.SSOAccountID), strings.TrimSpace(b.SSORoleName)
		}
		if mp.Kind == types.ModelProviderCustomEndpoint {
			if mp.Auth == nil {
				mp.Auth = &types.ProviderAuth{}
			}
			mp.Auth.Header = cmp.Or(strings.TrimSpace(mp.Auth.Header), defaultEndpointAuthHeader)
			mp.Auth.Format = cmp.Or(strings.TrimSpace(mp.Auth.Format), defaultEndpointAuthFormat)
		}
		for j := range mp.Harnesses {
			h := &mp.Harnesses[j]
			h.Harness, h.Model, h.Path = strings.TrimSpace(h.Harness), strings.TrimSpace(h.Model), strings.TrimSpace(h.Path)
			h.AuthHeader, h.AuthFormat = strings.TrimSpace(h.AuthHeader), strings.TrimSpace(h.AuthFormat)
		}
	}
	if p.Empty() {
		return nil
	}
	return p
}

// assignModelProviderUIDs gives each provider the UID its id holds in the stored
// block, or a freshly minted one. The UID is server-owned because every person's
// credential for the provider will be stored under it: a client that could
// choose one could aim a new provider at credentials given for another. Minted
// at random, so a deleted provider's UID is never reissued.
//
// A KIND change mints a fresh UID too: a credential was given for one kind of
// destination, and keeping the UID would let an anthropic_api_key provider
// become a custom_endpoint and send people's stored keys somewhere new. The old
// UID's credentials then belong to no provider.
func assignModelProviderUIDs(block, stored *types.ModelProviders) {
	if block == nil {
		return
	}
	held := map[string]types.ModelProvider{}
	if stored != nil {
		for _, p := range stored.Providers {
			held[p.ID] = p
		}
	}
	for i := range block.Providers {
		p := &block.Providers[i]
		p.UID = ""
		if old, ok := held[p.ID]; ok && old.Kind == p.Kind {
			p.UID = old.UID
		}
		p.UID = cmp.Or(p.UID, uuid.NewString())
	}
}

// validateModelProviders is the ONE write-boundary gate every door runs. A nil
// block is valid (today's behaviour), so the unconfigured case costs nothing.
func validateModelProviders(p *types.ModelProviders) error {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, mp := range p.Providers {
		if !modelProviderIDPattern.MatchString(mp.ID) {
			return fmt.Errorf(mp400ID, i, mp.ID)
		}
		if seen[mp.ID] {
			return fmt.Errorf(mp400DupID, mp.ID)
		}
		seen[mp.ID] = true
		if !mp.Kind.Valid() {
			return fmt.Errorf(mp400Kind, i, string(mp.Kind), strings.Join(types.ClosedModelProviderKindList(), ", "))
		}
		if utf8.RuneCountInString(mp.Name) > maxModelProviderName || strings.ContainsFunc(mp.Name, unicode.IsControl) {
			return fmt.Errorf(mp400Name, mp.ID, maxModelProviderName)
		}
		for _, check := range []func(types.ModelProvider) error{
			validateProviderAddress, validateProviderAuth, validateProviderBedrock, validateProviderHarnesses,
		} {
			if err := check(mp); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateProviderAddress holds BaseURL to the seven rules the boot gateway
// knobs already pass (validateOneLLMGateway), against the vendor host the kind
// would otherwise reach.
func validateProviderAddress(mp types.ModelProvider) error {
	switch {
	case mp.Kind.IsBedrock() && mp.BaseURL != "":
		return fmt.Errorf(mp400BaseURLBR, mp.ID)
	case mp.Kind == types.ModelProviderCustomEndpoint && mp.BaseURL == "":
		return fmt.Errorf(mp400BaseURLNeed, mp.ID)
	case mp.BaseURL == "":
		return nil
	}
	for _, public := range providerPublicHosts(mp.Kind) {
		if _, err := validateOneLLMGateway(public, mp.BaseURL, false); err != nil {
			return fmt.Errorf(mp400BaseURL, mp.ID, err)
		}
	}
	return nil
}

// validateProviderAuth: the header scheme is a custom endpoint's alone, and its
// two halves pass the same gates the boot gateway header/format knobs do.
func validateProviderAuth(mp types.ModelProvider) error {
	if mp.Kind != types.ModelProviderCustomEndpoint {
		if mp.Auth != nil {
			return fmt.Errorf(mp400AuthUnused, mp.ID)
		}
		return nil
	}
	if mp.Auth == nil {
		return nil
	}
	return validateHeaderScheme(mp.ID, "auth", mp.Auth.Header, mp.Auth.Format)
}

func validateHeaderScheme(id, where, header, format string) error {
	if header != "" && !egress.ValidHeaderName(header) {
		return fmt.Errorf(mp400Header, id, where, header)
	}
	if err := validInjectionFormat(format); err != nil {
		return fmt.Errorf(mp400Format, id, where, err)
	}
	return nil
}

// validateProviderBedrock: a Bedrock kind needs a region (it names the hosts a
// run reaches); bedrock_sso alone needs the start URL and may carry the pin,
// held to the same grammars the agent roster's own pin is.
func validateProviderBedrock(mp types.ModelProvider) error {
	b := mp.Bedrock
	if !mp.Kind.IsBedrock() {
		if b != nil {
			return fmt.Errorf(mp400BRUnused, mp.ID)
		}
		return nil
	}
	if b == nil || b.Region == "" {
		return fmt.Errorf(mp400BRNeed, mp.ID, string(mp.Kind))
	}
	if !awsSSORegionPattern.MatchString(b.Region) {
		return fmt.Errorf(mp400Region, mp.ID, b.Region)
	}
	if b.BaseURL != "" {
		if _, err := validateOneLLMGateway(bedrockRuntimeHost(b.Region), b.BaseURL, false); err != nil {
			return fmt.Errorf(mp400BRBaseURL, mp.ID, err)
		}
	}
	if mp.Kind != types.ModelProviderBedrockSSO {
		if b.SSOStartURL != "" || b.SSOAccountID != "" || b.SSORoleName != "" {
			return fmt.Errorf(mp400SSOUnused, mp.ID)
		}
		return nil
	}
	if b.SSOStartURL == "" {
		return fmt.Errorf(mp400StartURLNeed, mp.ID)
	}
	if err := validateSSOStartURL(b.SSOStartURL); err != nil {
		return fmt.Errorf(mp400StartURL, mp.ID, err)
	}
	return validateProviderPin(mp.ID, b)
}

func validateProviderPin(id string, b *types.BedrockSettings) error {
	switch {
	case b.SSOAccountID == "" && b.SSORoleName == "":
		return nil
	case b.SSOAccountID == "" || b.SSORoleName == "":
		return fmt.Errorf(mp400PinPair, id)
	case !awsAccountID.MatchString(b.SSOAccountID):
		return fmt.Errorf(mp400AccountID, id)
	case !iamRoleName.MatchString(b.SSORoleName):
		return fmt.Errorf(mp400RoleName, id)
	}
	return nil
}

// providerHarnessIDs is the catalog harnesses a provider may name: every row
// Wardyn wires a model credential for. "none" and custom image keys take no
// provider — the image brings its own.
func providerHarnessIDs() []string {
	var out []string
	for _, d := range harnessCatalog {
		if !d.NoManagedAuth {
			out = append(out, d.ID)
		}
	}
	return out
}

// validateProviderHarnesses: each entry names a harness a provider can serve, at
// most once, that the kind can actually drive — refused with the catalog's own
// verbatim protocol fact (harnessDef.ProviderTypes), never a second opinion.
func validateProviderHarnesses(mp types.ModelProvider) error {
	seen := map[string]bool{}
	for _, h := range mp.Harnesses {
		def, ok := harnessByID(h.Harness)
		if !ok || def.NoManagedAuth {
			return fmt.Errorf(mp400Harness, mp.ID, h.Harness, strings.Join(providerHarnessIDs(), ", "))
		}
		if seen[h.Harness] {
			return fmt.Errorf(mp400HarnessDup, mp.ID, h.Harness)
		}
		seen[h.Harness] = true
		if reason := def.ProviderTypes[string(mp.Kind)]; reason != "" {
			return fmt.Errorf(mp400Incompatible, mp.ID, reason)
		}
		if err := validateProviderHarness(mp, h); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderHarness(mp types.ModelProvider, h types.ProviderHarness) error {
	switch {
	case h.Model == "" && mp.Kind.IsBedrock():
		return fmt.Errorf(mp400ModelNeed, mp.ID, h.Harness)
	case h.Model != "" && !modelIDPattern.MatchString(h.Model):
		return fmt.Errorf(mp400Model, mp.ID, h.Harness, h.Model)
	}
	if mp.Kind != types.ModelProviderCustomEndpoint {
		if h.Path != "" || h.AuthHeader != "" || h.AuthFormat != "" {
			return fmt.Errorf(mp400EndpointOnly, mp.ID, h.Harness)
		}
		return nil
	}
	if !validProviderPath(mp.BaseURL, h.Path) {
		return fmt.Errorf(mp400Path, mp.ID, h.Harness, h.Path)
	}
	return validateHeaderScheme(mp.ID, h.Harness, h.AuthHeader, h.AuthFormat)
}

// validProviderPath: a Path extends BaseURL's path and may not change the host
// ("one provider, one egress host"). Empty means BaseURL itself.
func validProviderPath(base, path string) bool {
	if path == "" {
		return true
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsAny(path, "?#\\") || strings.ContainsFunc(path, unicode.IsSpace) ||
		strings.ContainsFunc(path, unicode.IsControl) || slices.Contains(strings.Split(path, "/"), "..") {
		return false
	}
	b, errB := url.Parse(base)
	j, errJ := url.Parse(base + path)
	return errB == nil && errJ == nil && j.Host == b.Host
}
