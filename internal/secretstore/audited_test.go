// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fallbackStore is a two-namespace store with the For fallback of the seam,
// reporting each row it reads the way the pg store does.
type fallbackStore struct {
	rows  map[[2]string][]byte // {owner, name} -> value; a nil value refuses
	owner string
}

func (f *fallbackStore) Name() string { return "fake" }

func (f *fallbackStore) Put(_ context.Context, name string, v []byte) error {
	f.rows[[2]string{f.owner, name}] = v
	return nil
}

func (f *fallbackStore) Delete(context.Context, string) error { return nil }

func (f *fallbackStore) List(context.Context) ([]string, error) { return nil, nil }

func (f *fallbackStore) For(owner string) Store { return &fallbackStore{rows: f.rows, owner: owner} }

func (f *fallbackStore) Get(ctx context.Context, name string) ([]byte, error) {
	for _, o := range []string{f.owner, ""} {
		v, ok := f.rows[[2]string{o, name}]
		if !ok {
			continue
		}
		NoteRow(ctx, Row{Store: "fake", Owner: o, Name: name, Ref: "fake:" + o + "/" + name})
		if v == nil {
			return nil, fmt.Errorf("row (%q, %q) refused", o, name)
		}
		return v, nil
	}
	return nil, fmt.Errorf("absent: %w", ErrNotFound)
}

type recorded struct{ got []types.AuditEvent }

func (r *recorded) Record(_ context.Context, ev types.AuditEvent) error {
	r.got = append(r.got, ev)
	return nil
}

func dataOf(t *testing.T, ev types.AuditEvent) map[string]string {
	t.Helper()
	var d map[string]string
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("event data %s: %v", ev.Data, err)
	}
	return d
}

func TestAuditedRecordsOneReadWithPurposeOwnerStoreAndRef(t *testing.T) {
	rec := &recorded{}
	st := Audited(&fallbackStore{rows: map[[2]string][]byte{{"", "git-pat"}: []byte("operator-value")}}, rec)

	v, err := st.For("alice").Get(WithPurpose(t.Context(), PurposeDispatch), "git-pat")
	if err != nil || string(v) != "operator-value" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("recorded %d events for one read, want 1", len(rec.got))
	}
	ev := rec.got[0]
	if ev.Action != "secret.read" || ev.Target != "git-pat" || ev.Outcome != "success" ||
		ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" {
		t.Errorf("event = %+v", ev)
	}
	want := map[string]string{"purpose": "dispatch", "owner": "alice", "row_owner": "", "store": "fake", "ref": "fake:/git-pat"}
	if d := dataOf(t, ev); fmt.Sprint(d) != fmt.Sprint(want) {
		t.Errorf("data = %v, want %v (owner is who asked, row_owner whose row was read)", d, want)
	}
	if strings.Contains(string(ev.Data), "operator-value") || strings.Contains(ev.Target, "operator-value") {
		t.Error("the event carries the value")
	}
}

func TestAuditedRecordsARefusedReadAsAFailureWithoutItsError(t *testing.T) {
	rec := &recorded{}
	st := Audited(&fallbackStore{rows: map[[2]string][]byte{{"bob", "k"}: nil}}, rec)

	if _, err := st.For("bob").Get(WithPurpose(t.Context(), PurposeBoot), "k"); err == nil {
		t.Fatal("refused row read without error")
	}
	if len(rec.got) != 1 || rec.got[0].Outcome != "failure" {
		t.Fatalf("recorded %+v, want one failure", rec.got)
	}
	if d := dataOf(t, rec.got[0]); d["ref"] != "fake:bob/k" || d["purpose"] != "boot" {
		t.Errorf("data = %v, want the refused row's ref and the purpose", d)
	}
	if strings.Contains(string(rec.got[0].Data), "refused") {
		t.Error("the event carries the store's error text")
	}
}

func TestAuditedRecordsNothingForAnAbsentRow(t *testing.T) {
	rec := &recorded{}
	st := Audited(&fallbackStore{rows: map[[2]string][]byte{}}, rec)
	if _, err := st.Get(WithPurpose(t.Context(), PurposeStatus), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound through the decorator", err)
	}
	if len(rec.got) != 0 {
		t.Fatalf("recorded %+v for a row that does not exist", rec.got)
	}
}

func TestAuditedStaysSilentForASiteThatRecordsItsOwnRead(t *testing.T) {
	rec := &recorded{}
	st := Audited(&fallbackStore{rows: map[[2]string][]byte{{"carol", "k"}: []byte("v")}}, rec)

	ctx, row := SiteAudited(WithPurpose(t.Context(), PurposeDispatch))
	if _, err := st.For("carol").Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 0 {
		t.Fatalf("the decorator recorded %+v for a SiteAudited read; the site's own event would make two", rec.got)
	}
	if *row != (Row{Store: "fake", Owner: "carol", Name: "k", Ref: "fake:carol/k"}) {
		t.Errorf("site row = %+v, want the row the Get read", *row)
	}

	// A purpose set under a site mark takes the read back.
	if _, err := st.For("carol").Get(WithPurpose(ctx, PurposeStatus), "k"); err != nil || len(rec.got) != 1 {
		t.Fatalf("Get = %v, recorded %d; the innermost mark decides", err, len(rec.got))
	}
}
