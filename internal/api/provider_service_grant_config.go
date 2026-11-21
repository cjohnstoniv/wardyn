// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A custom endpoint's service grant (types.ProviderServiceGrant): its write-time rules, its digest, and the
// name and shape of the one record that holds its client secret with the destination the secret was written
// for. The mint, the cache and the routes are provider_service_grant.go and its siblings.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	serviceGrantSecretPrefix = "wardyn-harness-svcgrant-"
	serviceGrantSecretSuffix = "-oauth"
	// serviceGrantMaxScopes bounds the scope list; serviceGrantMaxIdentities the listed services.
	serviceGrantMaxScopes     = 32
	serviceGrantMaxIdentities = 64
	serviceGrantMaxAudience   = 512

	mp400ServiceGrantType     = "model_providers: %q: oidc.service_grant.grant_type must be exactly %q"
	mp400ServiceGrantURL      = "model_providers: %q: oidc.service_grant.token_url: %w"
	mp400ServiceGrantClient   = "model_providers: %q: oidc.service_grant.client_id must be 1 to 256 printable characters with no spaces"
	mp400ServiceGrantScope    = "model_providers: %q: oidc.service_grant.scope must name 1 to %d values, separated by single spaces, with no control character"
	mp400ServiceGrantAudience = "model_providers: %q: oidc.service_grant.audience must be 1 to %d printable characters with no space"
	mp400ServiceGrantAuth     = "model_providers: %q: oidc.service_grant.client_auth must be %q or %q"
	mp400ServiceGrantServices = "model_providers: %q: oidc.service_grant.service_identities must list 1 to %d distinct service principals, each spelled exactly service:<name> in lower case"
)

// serviceIdentityNamePattern is the name part of a service principal (0157_invitations_service_identities.sql).
var serviceIdentityNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)

// serviceGrantUIDPattern is what a provider uid looks like (uuid.NewString); a name is composed only from one.
var serviceGrantUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// serviceGrantSecretName is the operator-namespace name a row's client secret record is kept under. It sits
// inside the wardyn-harness-*-oauth pattern every secret sink already refuses, so no policy, grant or the
// generic secrets API can name it. It panics on an uid that is not a generated one, as adoEntraSecretName does.
func serviceGrantSecretName(uid string) string {
	if !serviceGrantUIDPattern.MatchString(uid) {
		panic("api: serviceGrantSecretName called with an unvalidated provider uid")
	}
	return serviceGrantSecretPrefix + uid + serviceGrantSecretSuffix
}

// serviceGrantSecretUID is serviceGrantSecretName's inverse.
func serviceGrantSecretUID(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, serviceGrantSecretPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, serviceGrantSecretSuffix)
	return uid, ok && serviceGrantUIDPattern.MatchString(uid)
}

// normalizeServiceGrant trims the grant's free-text fields on their way to storage. It folds no case: token
// endpoint paths, client ids, scopes, audiences and principals can be case-sensitive and go out as written.
// The listed identities are not trimmed either: a padded entry is refused at the write, never stored mended.
func normalizeServiceGrant(g *types.ProviderServiceGrant) {
	if g == nil {
		return
	}
	g.GrantType, g.TokenURL, g.ClientID = strings.TrimSpace(g.GrantType), strings.TrimSpace(g.TokenURL), strings.TrimSpace(g.ClientID)
	g.Scope, g.Audience, g.ClientAuth = strings.TrimSpace(g.Scope), strings.TrimSpace(g.Audience), strings.TrimSpace(g.ClientAuth)
}

// validateServiceGrant holds a row's service grant to its rules. Always https for the token URL, whatever the
// test-endpoint posture says.
func validateServiceGrant(mp types.ModelProvider) error {
	g := mp.OIDC.ServiceGrant
	if g == nil {
		return nil
	}
	if g.GrantType != types.ServiceGrantTypeClientCreds {
		return atField("oidc.service_grant.grant_type", fmt.Errorf(mp400ServiceGrantType, mp.ID, types.ServiceGrantTypeClientCreds))
	}
	if g.TokenURL == "" {
		return atField("oidc.service_grant.token_url", fmt.Errorf(mp400ServiceGrantURL, mp.ID, errors.New("a token URL is required")))
	}
	if _, err := validateOneLLMGateway("", g.TokenURL, false); err != nil {
		return atField("oidc.service_grant.token_url", fmt.Errorf(mp400ServiceGrantURL, mp.ID, err))
	}
	if !oidcClientIDPattern.MatchString(g.ClientID) {
		return atField("oidc.service_grant.client_id", fmt.Errorf(mp400ServiceGrantClient, mp.ID))
	}
	if g.Scope != "" && !validServiceGrantScope(g.Scope) {
		return atField("oidc.service_grant.scope", fmt.Errorf(mp400ServiceGrantScope, mp.ID, serviceGrantMaxScopes))
	}
	if g.Audience != "" && !validServiceGrantAudience(g.Audience) {
		return atField("oidc.service_grant.audience", fmt.Errorf(mp400ServiceGrantAudience, mp.ID, serviceGrantMaxAudience))
	}
	if g.ClientAuth != "" && g.ClientAuth != types.ServiceGrantClientAuthBasic && g.ClientAuth != types.ServiceGrantClientAuthPost {
		return atField("oidc.service_grant.client_auth", fmt.Errorf(mp400ServiceGrantAuth, mp.ID, types.ServiceGrantClientAuthBasic, types.ServiceGrantClientAuthPost))
	}
	if !validServiceIdentities(g.ServiceIdentities) {
		return atField("oidc.service_grant.service_identities", fmt.Errorf(mp400ServiceGrantServices, mp.ID, serviceGrantMaxIdentities))
	}
	return nil
}

// validServiceGrantScope: values separated by single spaces, none empty, none holding a control character.
func validServiceGrantScope(scope string) bool {
	if strings.ContainsFunc(scope, unicode.IsControl) {
		return false
	}
	values := strings.Split(scope, " ")
	return len(values) <= serviceGrantMaxScopes && !slices.Contains(values, "")
}

func validServiceGrantAudience(a string) bool {
	return len(a) <= serviceGrantMaxAudience && !strings.ContainsFunc(a, func(r rune) bool { return r < 0x21 || r > 0x7e })
}

// validServiceIdentities: each entry exactly service:<name>, the name in the table's own pattern, no repeats.
func validServiceIdentities(ids []string) bool {
	if len(ids) == 0 || len(ids) > serviceGrantMaxIdentities {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		name, ok := strings.CutPrefix(id, types.ServicePrincipalPrefix)
		if !ok || !serviceIdentityNamePattern.MatchString(name) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// serviceGrantListed reports whether principal is one of the grant's listed service identities.
func serviceGrantListed(g *types.ProviderServiceGrant, principal string) bool {
	return g != nil && slices.Contains(g.ServiceIdentities, principal)
}

// serviceGrantDigest covers what a minted token stands for: the row's uid, its gateway address and header,
// and every grant field except the listed identities, which are read live at each resolve. A change to any
// of them makes a run's snapshot drift and a cached token unusable. "" for a row with no grant.
func serviceGrantDigest(p types.ModelProvider) string {
	if p.OIDC == nil || p.OIDC.ServiceGrant == nil {
		return ""
	}
	g := p.OIDC.ServiceGrant
	lanes := map[string][3]string{}
	for _, h := range p.Harnesses {
		lanes[h.Harness] = [3]string{h.Path, h.AuthHeader, h.AuthFormat}
	}
	format := ""
	if p.Auth != nil {
		format = p.Auth.Format
	}
	sum := sha256.Sum256(mustJSON([]any{p.UID, p.BaseURL, authHeader(p.Auth), format, lanes,
		g.GrantType, g.TokenURL, g.ClientID, g.Scope, g.Audience, g.ClientAuth}))
	return hex.EncodeToString(sum[:])
}

// serviceGrantRecord is the one stored value of a grant's secret: the client secret beside the destination
// it was written for, so the bytes sent and the destination they were written for come from one read.
type serviceGrantRecord struct {
	V            int    `json:"v"`
	TokenURL     string `json:"token_url"`
	ClientID     string `json:"client_id"`
	BaseURL      string `json:"base_url"`
	ClientSecret string `json:"client_secret"`
}

// parseServiceGrantRecord reads a stored record; ok is false for anything that is not this shape.
func parseServiceGrantRecord(raw []byte) (serviceGrantRecord, bool) {
	var rec serviceGrantRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.V != 1 || rec.TokenURL == "" || rec.ClientID == "" ||
		rec.BaseURL == "" || rec.ClientSecret == "" {
		return serviceGrantRecord{}, false
	}
	return rec, true
}

// maskableServiceSecret: the masker would not silently drop the value.
func maskableServiceSecret(v string) bool {
	return len(v) >= secretmask.MinLen && !secretmask.TrimmedTooShort([]byte(v))
}

// destinationMatches reports whether the record was written for exactly this row's grant and gateway.
func (r serviceGrantRecord) destinationMatches(p types.ModelProvider) bool {
	g := p.OIDC.ServiceGrant
	return g != nil && r.TokenURL == g.TokenURL && r.ClientID == g.ClientID && r.BaseURL == p.BaseURL
}
