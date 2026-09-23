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
	"unicode"

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

// countingStore counts the Gets that reach the store.
type countingStore struct {
	Store
	gets int
}

func (c *countingStore) Get(ctx context.Context, name string) ([]byte, error) {
	c.gets++
	return c.Store.Get(ctx, name)
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
	if rec.got[0].Target != "k" {
		t.Errorf("target = %q, want the bare row name", rec.got[0].Target)
	}
	if strings.Contains(string(rec.got[0].Data), "refused") || strings.Contains(rec.got[0].Target, "refused") {
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
	if *row != (Row{Store: "fake", Owner: "carol", Name: "k", Ref: "fake:carol/k", Found: true}) {
		t.Errorf("site row = %+v, want the row the Get read", *row)
	}

	// A purpose set under a site mark takes the read back.
	if _, err := st.For("carol").Get(WithPurpose(ctx, PurposeStatus), "k"); err != nil || len(rec.got) != 1 {
		t.Fatalf("Get = %v, recorded %d; the innermost mark decides", err, len(rec.got))
	}
}

// A Get whose caller set no purpose is refused before the store is read, and
// the attempt is on the record as a failure that says so — never a success
// with an empty purpose (rule 23: the guard pins the marks statically; this is
// the runtime backstop for a path it cannot see).
func TestAuditedRefusesAReadWithNoPurpose(t *testing.T) {
	rec := &recorded{}
	inner := &countingStore{Store: &fallbackStore{rows: map[[2]string][]byte{{"", "k"}: []byte("secret-value")}}}
	st := Audited(inner, rec)

	for _, ctx := range []context.Context{t.Context(), WithPurpose(t.Context(), "")} {
		v, err := st.Get(ctx, "k")
		if err == nil || errors.Is(err, ErrNotFound) || v != nil {
			t.Fatalf("unmarked Get = %q, %v; want a refusal that is not not-found", v, err)
		}
		if !strings.Contains(err.Error(), `"k"`) || strings.Contains(err.Error(), "secret-value") {
			t.Errorf("refusal %q must name the row and never carry the value", err)
		}
	}
	if inner.gets != 0 {
		t.Errorf("the store was read %d times for an unmarked Get", inner.gets)
	}
	if len(rec.got) != 2 {
		t.Fatalf("recorded %d events for two refused reads, want 2", len(rec.got))
	}
	ev := rec.got[0]
	want := map[string]string{"purpose": "unmarked", "owner": "", "store": "fake"}
	if d := dataOf(t, ev); ev.Outcome != "failure" || ev.Target != "k" || fmt.Sprint(d) != fmt.Sprint(want) {
		t.Errorf("recorded %+v with data %v, want a failure for k with %v", ev, d, want)
	}
}

// The row's owner and ref are text a database writer controls: each is bounded
// and stripped of non-printing runes before it reaches an audit sink. So is
// RecordRead's own owner argument — the boot conversion's migrate rows pass it
// row.Owner straight from secrets.owned_by (see convertSecretStore).
func TestAuditedBoundsRowTextFromTheStore(t *testing.T) {
	long := strings.Repeat("r", 4*auditTextMax)
	row := Row{Store: "pg", Owner: "eve\nforged‮", Name: "k\r", Ref: long, Found: true}
	rec := &recorded{}
	RecordRead(t.Context(), rec, PurposeStatus, "eve\nforged‮"+long, row, nil)

	d := dataOf(t, rec.got[0])
	for _, s := range []string{d["row_owner"], d["ref"], d["owner"], rec.got[0].Target} {
		if strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
			t.Errorf("recorded %q with a non-printing rune", s)
		}
	}
	if d["row_owner"] != "eve?forged?" || rec.got[0].Target != "k?" {
		t.Errorf("row_owner = %q, target = %q", d["row_owner"], rec.got[0].Target)
	}
	if !strings.HasPrefix(d["owner"], "eve?forged?") {
		t.Errorf("owner = %q, want it sanitized like row_owner", d["owner"])
	}
	if len(d["ref"]) > auditTextMax+len("…") {
		t.Errorf("ref recorded at %d bytes, want at most %d", len(d["ref"]), auditTextMax+len("…"))
	}
	if len(d["owner"]) > auditTextMax+len("…") {
		t.Errorf("owner recorded at %d bytes, want at most %d", len(d["owner"]), auditTextMax+len("…"))
	}
}

// A row the store found carries row_owner even when it has no ref (a legacy v0
// row the boot conversion read); one it did not find carries neither.
func TestRowAuditDataNamesTheOwnerOfEveryFoundRow(t *testing.T) {
	if d := (Row{Store: "pg", Owner: "bob", Name: "k", Found: true}).AuditData(); fmt.Sprint(d) != fmt.Sprint(map[string]string{"store": "pg", "row_owner": "bob"}) {
		t.Errorf("found v0 row = %v", d)
	}
	if d := (Row{Store: "pg", Name: "k"}).AuditData(); fmt.Sprint(d) != fmt.Sprint(map[string]string{"store": "pg"}) {
		t.Errorf("unfound row = %v", d)
	}
}
