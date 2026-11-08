// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestNotifyUnitAdmins_Recipients: the unit_admins target resolves user-subject grants on the run's
// unit and its ancestors to emails, and nothing else: no group grant, no grant on a sibling or a child,
// no email of anyone outside the chain.
func TestNotifyUnitAdmins_Recipients(t *testing.T) {
	h := newHarness(t, chanJSON("http://h.example.com/x", ""))
	ctx := context.Background()
	root, mid, leaf, sibling, child := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []struct{ id, parent uuid.UUID }{{root, uuid.Nil}, {mid, root}, {leaf, mid}, {sibling, mid}, {child, leaf}} {
		var parent, overlay any
		if u.parent != uuid.Nil {
			parent, overlay = u.parent, `{"allowed_domains":["pypi.org"]}`
		}
		if _, err := h.pool.Exec(ctx, `INSERT INTO governance_units (id, name, ceiling, limits, parent_id, overlay, created_by)
			VALUES ($1, $2, '{}', '{}', $3, $4, 'test')`, u.id, "u-"+u.id.String(), parent, overlay); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range [][2]string{{"sub-root", "Root@Example.com"}, {"sub-leaf", "leaf@example.com"}, {"sub-sib", "sib@example.com"}, {"sub-child", "child@example.com"}, {"sub-group", "member@example.com"}} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO people (principal, email, created_by) VALUES ($1, $2, 'test')`, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []struct {
		unit         uuid.UUID
		typ, subject string
	}{
		{root, "user", "sub-root"},         // ancestor, by sub: its person's email
		{mid, "user", "mid@example.com"},   // ancestor, by email with no people row: itself
		{leaf, "user", "LEAF@example.com"}, // the unit itself, by email in another case: deduplicated with its person
		{leaf, "user", "sub-leaf"},         // same person by sub
		{leaf, "group", "eng-leads"},       // a group is never expanded
		{leaf, "user", "no-address-sub"},   // neither a person nor an email
		{sibling, "user", "sub-sib"},       // not in the chain
		{child, "user", "sub-child"},       // below the unit
	} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO governance_unit_admins (id, unit_id, subject_type, subject, granted_by)
			VALUES ($1, $2, $3, $4, 'test')`, uuid.New(), g.unit, g.typ, g.subject); err != nil {
			t.Fatal(err)
		}
	}
	w := h.worker(nil, nil)
	got, err := w.UnitAdminEmails(ctx, &leaf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"leaf@example.com", "mid@example.com", "root@example.com"}; !slices.Equal(got, want) {
		t.Errorf("recipients = %v, want %v", got, want)
	}
	if got, err := w.UnitAdminEmails(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("no unit: %v, %v", got, err)
	}
}
