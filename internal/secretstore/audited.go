// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Purpose says why a Get reads a secret. The caller puts it in the context
// (WithPurpose) and Audited records it on the read's secret.read event.
type Purpose string

// The purposes of reads that Audited records. The injection sinks record their
// own secret.read (SiteAudited) and name their purposes there.
const (
	PurposeBoot         Purpose = "boot"
	PurposeBrokerMint   Purpose = "broker-mint"
	PurposeDispatch     Purpose = "dispatch"
	PurposeManagedToken Purpose = "managed-token"
	PurposeMigrate      Purpose = "migrate"
	PurposeSSORefresh   Purpose = "sso-refresh"
	PurposeADORefresh   Purpose = "ado-refresh"
	PurposeStatus       Purpose = "status"

	// PurposeUnmarked is recorded, as a failure, for a Get whose caller set no
	// purpose: Audited refuses it before the store is read.
	PurposeUnmarked Purpose = "unmarked"
)

// Row is the stored row one read opened: the store that holds it, the row's
// own owner (the operator's "" when a view fell back to it), its name, and the
// ref its value is kept under — for the pg store, the row's kek_id ("" on a
// legacy v0 row, which has none). Found says the store found the row, so Owner
// is the row's and not merely unset; NoteRow sets it.
type Row struct {
	Store, Owner, Name, Ref string
	Found                   bool
}

// auditTextMax bounds each row-sourced field of an audit event.
const auditTextMax = 512

// auditText makes text a database writer controls safe for an audit sink:
// every non-printing rune (control, format, line separator) becomes '?', and
// the result is cut to auditTextMax bytes.
func auditText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, s)
	if len(s) > auditTextMax {
		s = strings.ToValidUTF8(s[:auditTextMax], "") + "…"
	}
	return s
}

// AuditData is the row as a secret.read records it: store, and once the store
// found the row, row_owner and (when the row has one) ref. The owner and ref
// are the row's own text, so they are passed through auditText.
func (r Row) AuditData() map[string]string {
	d := map[string]string{}
	if r.Store != "" {
		d["store"] = r.Store
	}
	if r.Found {
		d["row_owner"] = auditText(r.Owner)
	}
	if r.Ref != "" {
		d["ref"] = auditText(r.Ref)
	}
	return d
}

type markKey struct{}

// mark is the context value every Get reads. site is set by SiteAudited; row,
// when set, is filled by the store with the row the Get read.
type mark struct {
	purpose Purpose
	site    bool
	row     *Row
}

func markOf(ctx context.Context) mark {
	m, _ := ctx.Value(markKey{}).(mark)
	return m
}

// WithPurpose marks ctx with why the reads under it happen.
func WithPurpose(ctx context.Context, p Purpose) context.Context {
	return context.WithValue(ctx, markKey{}, mark{purpose: p})
}

// SiteAudited marks ctx for a call site that records its own, richer
// secret.read (the injection sinks, with grant_id and jti): Audited stays
// silent for every Get under it, so each read is recorded once. The returned
// Row is filled by those Gets, so the site's event can name the store and ref.
func SiteAudited(ctx context.Context) (context.Context, *Row) {
	r := &Row{}
	return context.WithValue(ctx, markKey{}, mark{site: true, row: r}), r
}

// NoteRow is how a Store reports the row a Get is reading, before it opens the
// value, so a refusal is recorded against the row too. A Store calls it once
// per Get that finds a row.
func NoteRow(ctx context.Context, r Row) {
	r.Found = true
	if m := markOf(ctx); m.row != nil {
		*m.row = r
	}
	if g, ok := ctx.Value(grantReadKey{}).(grantRead); ok {
		*g.row = r
	}
}

// Scope names whose namespace a found row is in: "operator" for the
// operator's ("") row, "own" otherwise (a view reads no other owner's row).
// "" when no row was found.
func (r Row) Scope() string {
	switch {
	case !r.Found:
		return ""
	case r.Owner == "":
		return "operator"
	}
	return "own"
}

type grantReadKey struct{}

// grantRead is kept apart from mark: Audited replaces the mark on the way in,
// and this must reach the store unchanged.
type grantRead struct {
	ownOnly bool
	row     *Row
}

// GrantRead marks ctx for reads made on a credential grant's behalf. ownOnly
// (the grant's owner_only) removes the operator fallback from For(owner).Get:
// only the owner's own row can match. The returned Row is filled with the row
// each read opens, so the caller can record its Scope. It is not an audit
// mark; the read still needs WithPurpose or SiteAudited.
func GrantRead(ctx context.Context, ownOnly bool) (context.Context, *Row) {
	r := &Row{}
	return context.WithValue(ctx, grantReadKey{}, grantRead{ownOnly: ownOnly, row: r}), r
}

// OwnRowOnly reports whether reads under ctx must not fall back to the
// operator's row (GrantRead). A Store's Get honors it.
func OwnRowOnly(ctx context.Context) bool {
	g, _ := ctx.Value(grantReadKey{}).(grantRead)
	return g.ownOnly
}

// Audited wraps s so every Get records one secret.read on rec (owner, store,
// row ref, purpose) unless the context is SiteAudited. A Get finding no row
// records nothing. A Get with neither mark is refused before the store is
// read and recorded as a failure with purpose unmarked, so a read never goes
// unrecorded without saying why. Values are never recorded.
func Audited(s Store, rec audit.Recorder) Store {
	return &audited{inner: s, rec: rec}
}

type audited struct {
	inner Store
	rec   audit.Recorder
	owner string
}

func (a *audited) Name() string { return a.inner.Name() }

func (a *audited) Put(ctx context.Context, name string, value []byte) error {
	return a.inner.Put(ctx, name, value)
}

func (a *audited) Delete(ctx context.Context, name string) error { return a.inner.Delete(ctx, name) }

func (a *audited) List(ctx context.Context) ([]string, error) { return a.inner.List(ctx) }

func (a *audited) DeleteEverywhere(ctx context.Context, names []string) (int, error) {
	return a.inner.DeleteEverywhere(ctx, names)
}

// Holders is not audited: it reads which namespaces hold a row, never a value.
func (a *audited) Holders(ctx context.Context, names []string) (map[string][]string, error) {
	return a.inner.Holders(ctx, names)
}

// StoresExternally forwards the wrapped store's description of the external
// store its writes go to (the pg store in store mode), or "": metadata for the
// setup row, never a value.
func (a *audited) StoresExternally() string {
	if d, ok := a.inner.(interface{ StoresExternally() string }); ok {
		return d.StoresExternally()
	}
	return ""
}

// ErrNoExpirySweep is DeleteExpired's answer from a wrapper whose store cannot
// sweep: least retention is off, and the caller must say so rather than read
// it as nothing to delete.
var ErrNoExpirySweep = errors.New("secretstore: this store has no expiry sweep")

// DeleteExpired forwards the wrapped store's expiry sweep (pg Store.DeleteExpired),
// or answers ErrNoExpirySweep when it has none. Without it the daily sweep never
// reaches the store wardynd serves with, which is always wrapped.
func (a *audited) DeleteExpired(ctx context.Context) ([]Expired, error) {
	if sw, ok := a.inner.(interface {
		DeleteExpired(context.Context) ([]Expired, error)
	}); ok {
		return sw.DeleteExpired(ctx)
	}
	return nil, ErrNoExpirySweep
}

// MarkUsed, Metadata and MetadataEverywhere forward the wrapped store's row
// metadata (MetaStore), or answer ErrNoMetadata. Not audited: none reads a
// value.
func (a *audited) MarkUsed(ctx context.Context, name string) error {
	m, ok := a.inner.(MetaStore)
	if !ok {
		return ErrNoMetadata
	}
	return m.MarkUsed(ctx, name)
}

func (a *audited) Metadata(ctx context.Context, names []string) ([]Meta, error) {
	m, ok := a.inner.(MetaStore)
	if !ok {
		return nil, ErrNoMetadata
	}
	return m.Metadata(ctx, names)
}

func (a *audited) MetadataEverywhere(ctx context.Context, names []string) ([]Meta, error) {
	m, ok := a.inner.(MetaStore)
	if !ok {
		return nil, ErrNoMetadata
	}
	return m.MetadataEverywhere(ctx, names)
}

// KeyService forwards the wrapped store's description of the key service that
// wraps its writes (the pg store under WARDYN_KEK=transit), or "": metadata
// for the setup row, never a value.
func (a *audited) KeyService() string {
	if d, ok := a.inner.(interface{ KeyService() string }); ok {
		return d.KeyService()
	}
	return ""
}

func (a *audited) For(owner string) Store {
	return &audited{inner: a.inner.For(owner), rec: a.rec, owner: owner}
}

func (a *audited) Get(ctx context.Context, name string) ([]byte, error) {
	m := markOf(ctx)
	if m.site {
		return a.inner.Get(ctx, name)
	}
	row := &Row{Store: a.inner.Name(), Name: name}
	if m.purpose == "" {
		err := fmt.Errorf("secretstore: refused to read %q for owner %q: the read carries no audit purpose (secretstore.WithPurpose)", name, a.owner)
		RecordRead(ctx, a.rec, PurposeUnmarked, a.owner, *row, err)
		return nil, err
	}
	v, err := a.inner.Get(context.WithValue(ctx, markKey{}, mark{purpose: m.purpose, row: row}), name)
	if errors.Is(err, ErrNotFound) {
		return v, err
	}
	RecordRead(ctx, a.rec, m.purpose, a.owner, *row, err)
	return v, err
}

// RecordRead records the secret.read for one read of row: by Audited for a
// Get, and by a caller opening rows without Get (the boot conversion of
// legacy rows). owner is the namespace read for; err, when set, makes the
// outcome a failure. Only names and refs are recorded, never values or error
// text. A failed audit write is logged, not refused for.
func RecordRead(ctx context.Context, rec audit.Recorder, p Purpose, owner string, row Row, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	data := row.AuditData()
	data["purpose"], data["owner"] = string(p), auditText(owner)
	raw, _ := json.Marshal(data)
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "secret.read", Target: auditText(row.Name), Outcome: outcome, Data: raw,
	}
	if rerr := rec.Record(ctx, ev); rerr != nil {
		audit.LogWriteFailure(ctx, ev, rerr)
	}
}
