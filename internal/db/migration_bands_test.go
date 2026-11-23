// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The migration number bands (migration_bands.tsv): each release line numbers its new migrations from its own
// band, so a 0.10.x patch and a 0.11 change never take the same number, and a forward-ported file keeps its name.
// A database can then apply two bands in either order (Migrate applies unapplied files in lexical order, and a
// forward-ported file reaches an RC database after that database ran the newer band), which the both-orders PG
// test and scripts/lib/migration-overlap.py hold to one result.

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// migrationBand is one row of migration_bands.tsv. follows is the first prefix of the band complete before this
// band's first file exists (0 for none): this band then applies after it on every database, but only while that
// band's row is open=no (legalBandOrders).
type migrationBand struct {
	first, last int
	open        bool
	branches    []string
	follows     int
	tags        string
}

func (b migrationBand) label() string { return fmt.Sprintf("%04d-%04d", b.first, b.last) }

// readMigrationBands parses the table next to this file, or fails the test.
func readMigrationBands(t *testing.T) []migrationBand {
	t.Helper()
	data, err := os.ReadFile("migration_bands.tsv")
	if err != nil {
		t.Fatalf("read migration_bands.tsv: %v", err)
	}
	bands, err := parseMigrationBands(string(data))
	if err != nil {
		t.Fatalf("migration_bands.tsv: %v", err)
	}
	if problems := validateMigrationBands(bands); len(problems) > 0 {
		t.Fatalf("migration_bands.tsv: %s", strings.Join(problems, "; "))
	}
	return bands
}

func parseMigrationBands(text string) ([]migrationBand, error) {
	var out []migrationBand
	for i, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("line %d has %d tab-separated fields, want 6 (first last open branches follows tags)", i+1, len(f))
		}
		first, err1 := strconv.Atoi(f[0])
		last, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || len(f[0]) != 4 || len(f[1]) != 4 {
			return nil, fmt.Errorf("line %d: band %q-%q is not two 4-digit prefixes", i+1, f[0], f[1])
		}
		if f[2] != "yes" && f[2] != "no" {
			return nil, fmt.Errorf("line %d: open is %q, want yes or no", i+1, f[2])
		}
		b := migrationBand{first: first, last: last, open: f[2] == "yes", branches: strings.Split(f[3], ","), tags: f[5]}
		if f[4] != "-" {
			n, err := strconv.Atoi(f[4])
			if err != nil {
				return nil, fmt.Errorf("line %d: follows is %q, want a band's first prefix or -", i+1, f[4])
			}
			b.follows = n
		}
		out = append(out, b)
	}
	return out, nil
}

// validateMigrationBands lists what is wrong with a parsed table: bands out of order, overlapping or with a gap
// between them, a follows that names no lower band, a line branch that is a glob, a tags value that is not a
// tag glob, a branch glob open in two rows, or main open in other than exactly one row.
func validateMigrationBands(bands []migrationBand) []string {
	var problems []string
	if len(bands) == 0 {
		return []string{"no bands"}
	}
	if bands[0].first < 2 {
		problems = append(problems, fmt.Sprintf("the first band starts at %04d, leaving no pre-band sequence", bands[0].first))
	}
	openRows := map[string]int{}
	for i, b := range bands {
		if b.first > b.last {
			problems = append(problems, fmt.Sprintf("band %s ends before it starts", b.label()))
		}
		if i > 0 && b.first != bands[i-1].last+1 {
			problems = append(problems, fmt.Sprintf("band %s does not start right after band %s", b.label(), bands[i-1].label()))
		}
		if b.follows != 0 && !slices.ContainsFunc(bands[:i], func(o migrationBand) bool { return o.first == b.follows }) {
			problems = append(problems, fmt.Sprintf("band %s follows %04d, which is no lower band's first prefix", b.label(), b.follows))
		}
		if len(b.branches) == 0 || b.branches[0] == "" || strings.ContainsAny(b.branches[0], "*?[") {
			problems = append(problems, fmt.Sprintf("band %s's first branch %q must be the line's own branch, not a glob", b.label(), strings.Join(b.branches, ",")))
		}
		if b.tags != "-" && !strings.HasPrefix(b.tags, "v") {
			problems = append(problems, fmt.Sprintf("band %s's tags %q is neither - nor a v* tag glob", b.label(), b.tags))
		}
		if b.open {
			for _, g := range b.branches {
				openRows[g]++
			}
		}
	}
	if last := bands[len(bands)-1].last; last != 9999 {
		problems = append(problems, fmt.Sprintf("the last band ends at %04d, want 9999", last))
	}
	for _, g := range slices.Sorted(maps.Keys(openRows)) {
		if openRows[g] > 1 {
			problems = append(problems, fmt.Sprintf("branch %q is open in %d bands", g, openRows[g]))
		}
	}
	if openRows["main"] != 1 {
		problems = append(problems, fmt.Sprintf("main is open in %d bands, want 1", openRows["main"]))
	}
	return problems
}

// bandFirstOf is the first prefix of the band name's prefix falls in, or 0 for the pre-band sequence (and for a
// name without a 4-digit prefix).
func bandFirstOf(name string, bands []migrationBand) int {
	m := migrationNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	for _, b := range bands {
		if n >= b.first && n <= b.last {
			return b.first
		}
	}
	return 0
}

// checkMigrationPrefixes lists what breaks the numbering rule in names: every name is NNNN_slug.sql, no prefix
// repeats, the pre-band sequence starts at 0001, each band present starts at its first prefix, and neither has a
// gap. Bands may be empty, and a gap between two bands is allowed.
func checkMigrationPrefixes(names []string, bands []migrationBand) []string {
	var problems []string
	seen := map[int]string{}
	byBand := map[int][]int{} // band first prefix (0: pre-band) -> its prefixes
	for _, name := range names {
		m := migrationNameRe.FindStringSubmatch(name)
		if m == nil {
			problems = append(problems, fmt.Sprintf("migration %q does not match NNNN_slug.sql naming", name))
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if other, dup := seen[n]; dup {
			problems = append(problems, fmt.Sprintf("duplicate migration prefix %04d used by both %q and %q", n, other, name))
			continue
		}
		seen[n] = name
		byBand[bandFirstOf(name, bands)] = append(byBand[bandFirstOf(name, bands)], n)
	}
	if len(byBand[0]) == 0 {
		problems = append(problems, fmt.Sprintf("no migration below %04d", bands[0].first))
	}
	for _, key := range slices.Sorted(maps.Keys(byBand)) {
		ps := byBand[key]
		slices.Sort(ps)
		label, start := fmt.Sprintf("0001-%04d", bands[0].first-1), 1
		if key != 0 {
			i := slices.IndexFunc(bands, func(b migrationBand) bool { return b.first == key })
			label, start = bands[i].label(), key
		}
		if ps[0] != start {
			problems = append(problems, fmt.Sprintf("band %s starts at %04d, want %04d", label, ps[0], start))
		}
		for i := 1; i < len(ps); i++ {
			if ps[i] != ps[i-1]+1 {
				problems = append(problems, fmt.Sprintf("gap in band %s between %04d and %04d; prefixes must be contiguous within a band", label, ps[i-1], ps[i]))
			}
		}
	}
	return problems
}

// legalBandOrders lists, ascending order first, every order in which a database can meet the bands: each band
// after the band it follows, where a follows counts only while the followed band's row is open=no. While main
// still numbers from a band, a new file can land in it after the following band has started.
func legalBandOrders(bands []migrationBand) [][]migrationBand {
	var out [][]migrationBand
	var walk func(order, rest []migrationBand)
	walk = func(order, rest []migrationBand) {
		if len(rest) == 0 {
			out = append(out, order)
			return
		}
		for i, b := range rest {
			if slices.ContainsFunc(rest, func(o migrationBand) bool { return o.first == b.follows && !o.open }) {
				continue // b follows a closed band that is not placed yet
			}
			walk(append(slices.Clone(order), b), append(slices.Clone(rest[:i]), rest[i+1:]...))
		}
	}
	walk(nil, bands)
	return out
}

func bandOrderName(order []migrationBand) string {
	parts := make([]string, len(order))
	for i, b := range order {
		parts[i] = fmt.Sprintf("%04d", b.first)
	}
	return "pre-band>" + strings.Join(parts, ">")
}

func TestMigrationBandsTableIsWellFormed(t *testing.T) {
	bands := readMigrationBands(t)
	want := []struct {
		first, last, follows int
		tags, branch         string
	}{
		{300, 399, 0, "v0.10.*", "release/0.10"},
		{400, 599, 0, "-", "main"},
		{600, 699, 400, "v0.11.*", "release/0.11"},
		{700, 9999, 400, "-", "main"},
	}
	if len(bands) != len(want) {
		t.Fatalf("migration_bands.tsv has %d bands, want %d", len(bands), len(want))
	}
	for i, w := range want {
		b := bands[i]
		if b.first != w.first || b.last != w.last || b.follows != w.follows || b.tags != w.tags || b.branches[0] != w.branch {
			t.Errorf("band %d is %+v, want first %d last %d follows %d tags %q branch %q", i, b, w.first, w.last, w.follows, w.tags, w.branch)
		}
	}
}

func TestMigrationBandsTableRejectsBadRows(t *testing.T) {
	row := func(first, last, open, branches, follows, tags string) string {
		return strings.Join([]string{first, last, open, branches, follows, tags}, "\t")
	}
	older := row("0300", "0399", "yes", "release/0.10", "-", "v0.10.*")
	for _, tc := range []struct{ name, table, want string }{
		{"five fields", "0300\t0399\tyes\tmain\t-", "want 6"},
		{"gap between bands", older + "\n" + row("0450", "0599", "yes", "main", "-", "-"), "does not start right after"},
		{"overlapping bands", older + "\n" + row("0390", "0599", "yes", "main", "-", "-"), "does not start right after"},
		{"follows no band", older + "\n" + row("0400", "9999", "yes", "main", "0500", "-"), "no lower band"},
		{"glob as the own branch", row("0300", "0399", "yes", "train/0.10.*", "-", "v0.10.*") + "\n" + row("0400", "9999", "yes", "main", "-", "-"), "not a glob"},
		{"main open twice", row("0300", "0399", "yes", "main", "-", "-") + "\n" + row("0400", "9999", "yes", "main", "-", "-"), `branch "main" is open in 2 bands`},
		{"last band short of 9999", older + "\n" + row("0400", "0799", "yes", "main", "-", "-"), "want 9999"},
		{"open is maybe", row("0300", "9999", "maybe", "main", "-", "-"), "want yes or no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bands, err := parseMigrationBands(tc.table + "\n")
			var problems []string
			if err != nil {
				problems = []string{err.Error()}
			} else {
				problems = validateMigrationBands(bands)
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("problems %q do not contain %q", problems, tc.want)
			}
		})
	}
}

func TestMigrationPrefixBandRules(t *testing.T) {
	bands := readMigrationBands(t)
	nums := func(prefixes ...int) []string {
		out := make([]string, len(prefixes))
		for i, n := range prefixes {
			out[i] = fmt.Sprintf("%04d_m.sql", n)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		names []string
		want  []string // every substring of the one problem; none: no problem
	}{
		{"pre-band only", nums(1, 2, 3), nil},
		{"pre-band and one band file", nums(1, 2, 3, 400), nil},
		{"every band", nums(1, 2, 3, 300, 301, 400, 600, 700), nil},
		{"gap inside a band", nums(1, 2, 3, 300, 302), []string{"gap in band 0300-0399", "0300", "0302"}},
		{"late start in a band", nums(1, 2, 3, 401), []string{"band 0400-0599 starts at 0401, want 0400"}},
		{"duplicate inside a band", append(nums(1, 2, 3), "0300_a.sql", "0300_b.sql"), []string{"duplicate migration prefix 0300"}},
		{"gap in the pre-band sequence", nums(1, 3), []string{"gap in band 0001-0299"}},
		{"pre-band late start", nums(2), []string{"band 0001-0299 starts at 0002, want 0001"}},
		{"bad name", append(nums(1), "0300_Bad-Name.sql"), []string{"does not match NNNN_slug.sql"}},
		{"no pre-band file", nums(300), []string{"no migration below 0300"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkMigrationPrefixes(tc.names, bands)
			if tc.want == nil {
				if len(problems) > 0 {
					t.Errorf("want no problem, got %q", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("want one problem containing %q, got %q", tc.want, problems)
			}
			for _, s := range tc.want {
				if !strings.Contains(problems[0], s) {
					t.Errorf("problem %q does not contain %q", problems[0], s)
				}
			}
		})
	}
}

func TestLegalBandOrders(t *testing.T) {
	// position is where the band starting at first sits in order, or -1.
	position := func(order []migrationBand, first int) int {
		return slices.IndexFunc(order, func(b migrationBand) bool { return b.first == first })
	}
	distinct := func(orders [][]migrationBand) int {
		seen := map[string]bool{}
		for _, o := range orders {
			seen[bandOrderName(o)] = true
		}
		return len(seen)
	}

	bands := readMigrationBands(t)
	// The real table, with the 0400 row open: both follows are void, so every permutation is legal.
	if !bands[1].open {
		t.Fatalf("the 0400 row is closed; the 0.12 commit that closes it must change this test to expect 8 orders on the real table")
	}
	orders := legalBandOrders(bands)
	if len(orders) != 24 || distinct(orders) != 24 {
		t.Errorf("with 0400 open: %d orders (%d distinct), want 24", len(orders), distinct(orders))
	}
	if got, want := bandOrderName(orders[0]), "pre-band>0300>0400>0600>0700"; got != want {
		t.Errorf("first order is %s, want the ascending %s", got, want)
	}
	if !slices.ContainsFunc(orders, func(o []migrationBand) bool { return position(o, 400) < position(o, 300) }) {
		t.Errorf("no order puts 0400 before 0300, which the control subtest needs")
	}
	if !slices.ContainsFunc(orders, func(o []migrationBand) bool { return position(o, 600) < position(o, 400) }) {
		t.Errorf("no order puts 0600 before 0400 while 0400 is open")
	}

	// The same table once the 0.12 commit closes the 0400 row: 0400 comes before 0600 and 0700.
	closed := slices.Clone(bands)
	closed[1].open = false
	closedOrders := legalBandOrders(closed)
	if len(closedOrders) != 8 || distinct(closedOrders) != 8 {
		t.Errorf("with 0400 closed: %d orders (%d distinct), want 8", len(closedOrders), distinct(closedOrders))
	}
	if got, want := bandOrderName(closedOrders[0]), "pre-band>0300>0400>0600>0700"; got != want {
		t.Errorf("first closed order is %s, want the ascending %s", got, want)
	}
	for _, o := range closedOrders {
		if position(o, 400) > position(o, 600) || position(o, 400) > position(o, 700) {
			t.Errorf("%s runs 0400 after 0600 or 0700 although both follow the closed 0400", bandOrderName(o))
		}
	}

	// A chain where each band follows the one before: one order once every band but the last is closed.
	chain := []migrationBand{{first: 1}, {first: 2, follows: 1}, {first: 3, follows: 2}, {first: 4, follows: 3, open: true}}
	if got := len(legalBandOrders(chain)); got != 1 {
		t.Errorf("a closed chain gives %d orders, want 1", got)
	}
	chain[0].open = true
	if got := len(legalBandOrders(chain)); got <= 1 {
		t.Errorf("a chain whose first band is open gives %d orders, want more than 1", got)
	}
}
