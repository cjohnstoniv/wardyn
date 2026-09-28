// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Stored-credential metadata (credential-storage design CS-6): the sinks stamp
// when a person's credential was last used for a run, admins read who holds a
// credential for which model provider, and each person reads their own on
// provider_access. Every answer comes from the rows' metadata columns — no
// value, data key or store reference is read to build one.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// credInventoryNoMeta is the inventory's 503 when the configured secret store
// keeps no credential metadata at all (design packet F, canon): the admin
// view's Stored credentials page shows this sentence verbatim in its empty
// card, with no Retry — retrying changes nothing about which store is
// configured.
const credInventoryNoMeta = "This deployment's secret store keeps no credential metadata, so there is nothing to list."

// The inventory's states. Metadata alone cannot say a credential still works
// (dispatch finds that out), only whether it is stored and past its expiry.
const (
	credStateStored  = "stored"
	credStateExpired = "expired"
)

// providerCredentialName is the one row a person's credential for p lives in
// under p's current kind; rule 8 purges the others when the kind changes.
func providerCredentialName(p types.ModelProvider) string {
	switch p.Kind {
	case types.ModelProviderBedrockSSO:
		return providerSecretName(p.UID, providerSSOPart)
	case types.ModelProviderAnthropicSubscription:
		return providerSecretName(p.UID, providerOAuthPart)
	default:
		return providerSecretName(p.UID, providerKeyPart)
	}
}

// stampCredentialUse records that a run's sink just used owner's own stored
// name (last_used_at; the store throttles it to once a minute per row). The
// resolve has already succeeded, so a failed stamp is logged, never refused.
func (s *Server) stampCredentialUse(ctx context.Context, owner, name string) {
	if s.cfg.Secrets == nil || owner == "" {
		return
	}
	m, ok := s.cfg.Secrets.For(owner).(secretstore.MetaStore)
	if !ok {
		return
	}
	if err := m.MarkUsed(ctx, name); err != nil && !errors.Is(err, secretstore.ErrNoMetadata) {
		slog.WarnContext(ctx, "wardynd: recording a credential's last use failed", slog.Any("err", err))
	}
}

// attachOwnCredentialMeta adds, to each provider_access row, when the caller
// stored their credential for it and when a run last used it. Read from the
// caller's OWN view only, so no other person's row can reach it. A read
// failure leaves the rows as graded: the metadata is additive.
func (s *Server) attachOwnCredentialMeta(ctx context.Context, sc types.SiteConfig, rows []SetupProviderAccess, owner string) {
	if s.cfg.Secrets == nil || len(rows) == 0 || s.providerAccessMechanism(owner) || previewHidesOwnCredential(ctx) {
		return
	}
	m, ok := s.cfg.Secrets.For(owner).(secretstore.MetaStore)
	if !ok {
		return
	}
	at := map[string]int{}
	names := make([]string, 0, len(rows))
	for i, row := range rows {
		if p, ok := modelProviderByID(sc.ModelProviders, row.Provider); ok {
			at[providerCredentialName(p)] = i
			names = append(names, providerCredentialName(p))
		}
	}
	metas, err := m.Metadata(ctx, names)
	if err != nil {
		return
	}
	for _, md := range metas {
		if i, ok := at[md.Name]; ok {
			added := md.AddedAt
			rows[i].AddedAt, rows[i].LastUsedAt = &added, md.LastUsedAt
		}
	}
}

// credentialInventoryRow is one person's stored credential for one provider.
//
// Email and ProviderName (design F-2, packet F) are added alongside Person
// (an opaque OIDC subject) and Provider (an id): neither is secret, and
// without them a security_admin — who has no route to the provider roster or
// an identity directory — could read only ids, which nobody can offboard
// from. Both are additive projections of data this route's tier already
// reaches elsewhere (knownPrincipals, the stored model-provider block); no
// new read and no widened access.
type credentialInventoryRow struct {
	Person       string     `json:"person"`
	Email        string     `json:"email,omitempty"`
	Provider     string     `json:"provider"`
	ProviderName string     `json:"provider_name,omitempty"`
	State        string     `json:"state"`
	Store        string     `json:"store"`
	AddedAt      time.Time  `json:"added_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// credentialInventoryCounts: distinct people holding any credential, the rows,
// and per provider id how many people hold one (0 included).
type credentialInventoryCounts struct {
	People      int            `json:"people"`
	Credentials int            `json:"credentials"`
	ByProvider  map[string]int `json:"by_provider"`
}

type credentialInventory struct {
	Credentials []credentialInventoryRow  `json:"credentials"`
	Counts      credentialInventoryCounts `json:"counts"`
}

// handleCredentialInventory lists, for every configured model provider, each
// person who holds a credential of their own for it: its state, where it is
// stored, when it was added and last used. Admin or security_admin (design
// K5-A: offboarding and incident response are both tiers' jobs); a member is
// refused at the group. The operator namespace is never listed.
//
//	GET /api/v1/model-providers/credentials
func (s *Server) handleCredentialInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	inv := credentialInventory{Credentials: []credentialInventoryRow{}, Counts: credentialInventoryCounts{ByProvider: map[string]int{}}}
	providerOf := map[string]string{}
	providerNames := map[string]string{}
	for _, p := range storedModelProviders(sc).Providers {
		inv.Counts.ByProvider[p.ID] = 0
		providerOf[providerCredentialName(p)] = p.ID
		providerNames[p.ID] = p.Name
	}
	if len(providerOf) > 0 {
		m, ok := s.cfg.Secrets.(secretstore.MetaStore)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, credInventoryNoMeta)
			return
		}
		metas, err := m.MetadataEverywhere(ctx, sortedKeys(providerOf))
		if errors.Is(err, secretstore.ErrNoMetadata) {
			writeError(w, http.StatusServiceUnavailable, credInventoryNoMeta)
			return
		}
		if err != nil {
			writeServerError(w, r, "read credential metadata", err)
			return
		}
		inv.fill(metas, providerOf, providerNames, s.emailsByPrincipal(ctx), s.cfg.Now())
	}
	writeJSON(w, http.StatusOK, inv)
}

// emailsByPrincipal pairs each principal ?owner= already resolves against
// (knownPrincipals) with its known email, first pairing wins. Used to add
// credentialInventoryRow.Email (design F-2) — the SAME directory, no new
// read, and no principal without a paired email gets one invented.
func (s *Server) emailsByPrincipal(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, p := range s.knownPrincipals(ctx) {
		if p.email == "" {
			continue
		}
		if _, ok := out[p.principal]; !ok {
			out[p.principal] = p.email
		}
	}
	return out
}

func (inv *credentialInventory) fill(metas []secretstore.Meta, providerOf, providerNames, emailOf map[string]string, now time.Time) {
	people := map[string]bool{}
	for _, md := range metas {
		id, ok := providerOf[md.Name]
		if !ok || md.Owner == "" {
			continue
		}
		state := credStateStored
		if md.ExpiresAt != nil && !md.ExpiresAt.After(now) {
			state = credStateExpired
		}
		inv.Credentials = append(inv.Credentials, credentialInventoryRow{
			Person: md.Owner, Email: emailOf[md.Owner], Provider: id, ProviderName: providerNames[id],
			State: state, Store: md.Store,
			AddedAt: md.AddedAt, LastUsedAt: md.LastUsedAt, ExpiresAt: md.ExpiresAt,
		})
		inv.Counts.ByProvider[id]++
		people[md.Owner] = true
	}
	inv.Counts.People, inv.Counts.Credentials = len(people), len(inv.Credentials)
}
