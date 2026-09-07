// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// THE PREMISE MIGRATION 0061'S INDEX RESTS ON.
//
// The index refuses two Wardyn-named drives whose names fold to one DriveSlug.
// That is the right namespace only while the minted object name is a function of
// (slug, home) and nothing else — no drive id, no backend, no discriminator of
// any kind. If a later change adds one, two drives with one slug stop colliding
// and the index is refusing legal rows; if it removes the slug, the index is
// guarding the wrong column. Either way 0061 has to be revisited, and this is
// what says so.
//
// It also carries the collision itself, from the finding: distinct names that
// UNIQUE(name) admits and that mint ONE object.

import "testing"

// slugCollisionPairs are name pairs UNIQUE(name) admits as different and
// DriveSlug folds together — case, punctuation, and the two combined.
var slugCollisionPairs = [][2]string{
	{"Corp NAS", "corp nas"},
	{"Corp NAS (eng)", "corp-nas-eng"},
	{"corp nas", "  Corp   NAS! "},
}

func TestMintedObjectNameIsAFunctionOfTheDriveSlug(t *testing.T) {
	const home = "bsmith"
	for _, b := range DriveBackends {
		if !DriveObjectNamedByWardyn(b) {
			continue
		}
		for _, p := range slugCollisionPairs {
			a := UserDrive{Name: p[0], Backend: b}
			z := UserDrive{Name: p[1], Backend: b}
			if DriveSlug(a.Name) != DriveSlug(z.Name) {
				t.Fatalf("%q and %q no longer fold to one slug (%q vs %q); this pair is the collision migration "+
					"0061's index exists for, so re-derive the pairs rather than deleting the test",
					p[0], p[1], DriveSlug(a.Name), DriveSlug(z.Name))
			}
			an, zn := DriveObjectName(a, home), DriveObjectName(z, home)
			if an != zn {
				t.Errorf("on %s, %q -> %q and %q -> %q. The minted name now separates two drives that share a slug, "+
					"so migration 0061's UNIQUE index on name_slug is refusing rows that no longer collide — revisit "+
					"it rather than leaving a rule nothing needs", b, p[0], an, p[1], zn)
			}
			if want := driveObjectPrefix + DriveSlug(a.Name) + "-" + home; an != want {
				t.Errorf("on %s the minted name is %q, not %q: it is no longer (prefix, slug, home) alone, and "+
					"name_slug is no longer the column that decides who collides with whom", b, an, want)
			}
		}
	}
}

// TestDriveSlugIsStableForTheColumn pins the fold's actual output, because the
// store WRITES it into user_drives.name_slug and migration 0061 backfilled
// existing rows with an equivalent SQL expression. A change here is a change to
// stored data, not only to a computation.
func TestDriveSlugIsStableForTheColumn(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Corp NAS", "corp-nas"},
		{"  Corp   NAS! ", "corp-nas"},
		{"Corp NAS (eng)", "corp-nas-eng"},
		{"corp-nas-eng", "corp-nas-eng"},
		{"---", ""},
		{"Ünïcode Drive", "n-code-drive"},
		// THE ONE CLASS WHERE 0061's SQL BACKFILL DIFFERS, pinned so it is a
		// known quantity rather than a surprise: Go's strings.ToLower maps
		// U+0130 into ASCII, while the backfill's lower(... COLLATE "C") lowers
		// ASCII only and the fold then replaces the character — "ice" here,
		// "ce" there. It matters for exactly one thing (a row written before
		// 0061 and never rewritten since) and self-heals the next time the
		// drive is written, because Go writes the column from then on.
		{"İce", "ice"},
		// The cap lands mid-run, and the trailing "-" it would leave is trimmed
		// AFTER the cut — the order migration 0061's backfill expression copies.
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!bbb", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	} {
		if got := DriveSlug(tc.in); got != tc.want {
			t.Errorf("DriveSlug(%q) = %q, want %q — user_drives.name_slug holds this value for every drive already "+
				"written, and 0061's backfill computed it in SQL; changing the fold silently re-partitions who "+
				"collides with whom", tc.in, got, tc.want)
		}
	}
}
