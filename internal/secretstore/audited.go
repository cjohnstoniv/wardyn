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
	if m := markOf(ctx); m.row != nil {
		r.Found = true
		*m.row = r
	}
}

// Audited wraps s so that every Get records one secret.read on rec, carrying
// the owner, the store, the row's ref and the context's purpose — unless the
// context is SiteAudited. A Get that finds no row records nothing: no value
// was read. A Get with neither mark is refused before the store is read, and
// recorded as a failure with purpose unmarked, so a read never goes on record
// without saying why. Values are never recorded.
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
// Get, and by a caller that opens rows without Get (the boot conversion of
// legacy rows). owner is the namespace the read was made for; err, when set,
// makes the outcome a failure. Only names and refs are recorded, never values
// or error text. A failed audit write is logged (wardynd's recorder chain also
// spools it); the read is not refused for it.
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
