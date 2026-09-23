// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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
)

// Row is the stored row one read opened: the store that holds it, the row's
// own owner (the operator's "" when a view fell back to it), its name, and the
// ref its value is kept under — for the pg store, the row's kek_id ("" on a
// legacy v0 row, which has none).
type Row struct {
	Store, Owner, Name, Ref string
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
		*m.row = r
	}
}

// Audited wraps s so that every Get records one secret.read on rec, carrying
// the owner, the store, the row's ref and the context's purpose — unless the
// context is SiteAudited. A Get that finds no row records nothing: no value
// was read. Values are never recorded.
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

func (a *audited) For(owner string) Store {
	return &audited{inner: a.inner.For(owner), rec: a.rec, owner: owner}
}

func (a *audited) Get(ctx context.Context, name string) ([]byte, error) {
	m := markOf(ctx)
	if m.site {
		return a.inner.Get(ctx, name)
	}
	row := &Row{Store: a.inner.Name(), Name: name}
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
// or error text.
func RecordRead(ctx context.Context, rec audit.Recorder, p Purpose, owner string, row Row, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	data := map[string]string{"purpose": string(p), "owner": owner, "store": row.Store}
	if row.Ref != "" {
		data["row_owner"], data["ref"] = row.Owner, row.Ref
	}
	raw, _ := json.Marshal(data)
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "secret.read", Target: row.Name, Outcome: outcome, Data: raw,
	}
	if rerr := rec.Record(ctx, ev); rerr != nil {
		audit.LogWriteFailure(ctx, ev, rerr)
	}
}
