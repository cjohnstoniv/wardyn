// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

// fakeTransit is a kek.KEK over its own random key, standing in for one Transit
// key: every wrap is bound to its row and to this key's id.
type fakeTransit struct {
	id  string
	key []byte
}

func newFakeTransit(name string) *fakeTransit {
	k := make([]byte, kek.DEKSize)
	_, _ = rand.Read(k)
	return &fakeTransit{id: kek.TransitIDPrefix + "transit/" + name, key: k}
}

func (f *fakeTransit) ID() string { return f.id }
func (f *fakeTransit) Wrap(_ context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, f.id)
	if err != nil {
		return nil, err
	}
	return kek.Seal(f.key, dek, aad)
}
func (f *fakeTransit) Unwrap(_ context.Context, w []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, f.id)
	if err != nil {
		return nil, err
	}
	return kek.Open(f.key, w, aad)
}

// domains builds a manager over two domains, a and b, beside the default key.
func domains(t *testing.T, pool *pgxpool.Pool, def, a, b *fakeTransit) (*subjectkey.Manager, *keydomain.Service) {
	t.Helper()
	svc := keydomain.NewService(pool, []string{"a", "b"})
	byDomain := map[string]*fakeTransit{keydomain.Default: def, "a": a, "b": b}
	return subjectkey.New(pool, subjectkey.Resolver{
		Writer: func(d string) (kek.KEK, error) {
			if k := byDomain[d]; k != nil {
				return k, nil
			}
			return nil, fmt.Errorf("no domain %q", d)
		},
		Reader: func(d, id string) (kek.KEK, error) {
			if k := byDomain[d]; k != nil && k.id == id {
				return k, nil
			}
			return nil, fmt.Errorf("domain %q does not reach %q", d, id)
		},
		Domain: svc.Domain,
	}), svc
}

type generation struct {
	version               int
	domain, kekID         string
	superseded, destroyed bool
}

func generations(t *testing.T, pool *pgxpool.Pool, owner string) []generation {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT version, domain, kek_id, superseded_at IS NOT NULL, destroyed_at IS NOT NULL
		FROM principal_keys WHERE owner=$1 AND purpose='cred' ORDER BY version`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []generation
	for rows.Next() {
		var g generation
		if err := rows.Scan(&g.version, &g.domain, &g.kekID, &g.superseded, &g.destroyed); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

// A reassignment gives the next write a new generation in the new domain; the
// old generation stays in its domain, readable, and is never re-wrapped.
func TestManager_ReassignmentAppliesToTheNextGeneration(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	def, a, b := newFakeTransit("default"), newFakeTransit("a"), newFakeTransit("b")
	m, svc := domains(t, pool, def, a, b)
	const alice = "alice@example.com"

	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: alice, Domain: "a", SetBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	v1, key1, err := m.Current(t.Context(), alice, subjectkey.PurposeCred)
	if err != nil || v1 != 1 {
		t.Fatalf("first write = (%d, %v), want generation 1", v1, err)
	}
	// The same domain again is the same generation.
	if v, key, err := m.Current(t.Context(), alice, subjectkey.PurposeCred); err != nil || v != 1 || !bytes.Equal(key, key1) {
		t.Fatalf("second write in the same domain = (%d, %v)", v, err)
	}
	var wrapped1 []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_key FROM principal_keys WHERE owner=$1 AND version=1`, alice).Scan(&wrapped1); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: alice, Domain: "b", SetBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	v2, key2, err := m.Current(t.Context(), alice, subjectkey.PurposeCred)
	if err != nil || v2 != 2 || bytes.Equal(key1, key2) {
		t.Fatalf("write after the reassignment = (%d, %v), want a new key as generation 2", v2, err)
	}
	got := generations(t, pool, alice)
	want := []generation{
		{version: 1, domain: "a", kekID: a.id, superseded: true},
		{version: 2, domain: "b", kekID: b.id},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("generations = %+v; want %+v", got, want)
	}
	// The old generation is untouched: same bytes, still opens, still domain a.
	var again []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_key FROM principal_keys WHERE owner=$1 AND version=1`, alice).Scan(&again); err != nil || !bytes.Equal(again, wrapped1) {
		t.Fatalf("generation 1's wrap changed after the reassignment (%v)", err)
	}
	if old, err := m.Key(t.Context(), alice, subjectkey.PurposeCred, 1); err != nil || !bytes.Equal(old, key1) {
		t.Fatalf("generation 1 after the reassignment = (%v), want its original key", err)
	}

	// Back to a: a new generation 3 in a; nothing is moved or reused.
	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: alice, Domain: "a", SetBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Current(t.Context(), alice, subjectkey.PurposeCred); err != nil || v != 3 {
		t.Fatalf("write after assigning back = (%d, %v), want generation 3", v, err)
	}
	if got := generations(t, pool, alice); len(got) != 3 || !got[0].superseded || !got[1].superseded || got[2].superseded || got[2].domain != "a" {
		t.Fatalf("generations = %+v", got)
	}

	// An erase tombstones every generation, the superseded ones too.
	gens, err := m.Destroy(t.Context(), alice, subjectkey.PurposeCred)
	if err != nil || len(gens) != 3 {
		t.Fatalf("Destroy = (%v, %v), want three generations", gens, err)
	}
	for v := 1; v <= 3; v++ {
		if _, err := m.Key(t.Context(), alice, subjectkey.PurposeCred, v); !errors.Is(err, subjectkey.ErrDataLoss) {
			t.Fatalf("generation %d after Destroy = %v, want ErrDataLoss", v, err)
		}
	}
}

// A database dump plus one domain's key opens that domain's keys and not
// another's: a wrap made in b does not unwrap under a, nor under a's key with
// the domain label changed.
func TestManager_OneDomainsKeyOpensOnlyThatDomain(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	def, a, b := newFakeTransit("default"), newFakeTransit("a"), newFakeTransit("b")
	m, svc := domains(t, pool, def, a, b)
	for owner, domain := range map[string]string{"in-a": "a", "in-b": "b"} {
		if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: owner, Domain: domain, SetBy: "admin"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.Current(t.Context(), owner, subjectkey.PurposeCred); err != nil {
			t.Fatal(err)
		}
	}
	// The attacker has the dump (every principal_keys row) and domain a's key.
	var wrappedB []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_key FROM principal_keys WHERE owner='in-b'`).Scan(&wrappedB); err != nil {
		t.Fatal(err)
	}
	var wrappedA []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_key FROM principal_keys WHERE owner='in-a'`).Scan(&wrappedA); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Unwrap(t.Context(), wrappedA, kek.PrincipalBind("in-a", "cred", 1, "a")); err != nil {
		t.Fatalf("domain a's key does not open a's own row: %v", err)
	}
	for name, bind := range map[string]map[string]string{
		"b's row under its own binding":        kek.PrincipalBind("in-b", "cred", 1, "b"),
		"b's row relabelled as domain a":       kek.PrincipalBind("in-b", "cred", 1, "a"),
		"b's row relabelled as the owner in-a": kek.PrincipalBind("in-a", "cred", 1, "b"),
	} {
		if _, err := a.Unwrap(t.Context(), wrappedB, bind); err == nil {
			t.Errorf("domain a's key opened %s", name)
		}
	}
	// A manager that holds only a's key reads a's keys and refuses b's.
	onlyA := subjectkey.New(pool, subjectkey.Resolver{
		Writer: func(string) (kek.KEK, error) { return a, nil },
		Reader: func(d, id string) (kek.KEK, error) {
			if d == "a" && id == a.id {
				return a, nil
			}
			return nil, fmt.Errorf("domain %q is not reachable with this key", d)
		},
	})
	if _, err := onlyA.Key(t.Context(), "in-a", subjectkey.PurposeCred, 1); err != nil {
		t.Fatalf("a-only manager on a's key: %v", err)
	}
	// A key this process cannot reach is unavailable to it, never destroyed: a masking read aborts
	// on it instead of fencing the owner's runs.
	if _, err := onlyA.Key(t.Context(), "in-b", subjectkey.PurposeCred, 1); !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, subjectkey.ErrDataLoss) {
		t.Fatalf("a-only manager on b's key = %v, want ErrUnavailable and not data loss", err)
	}
}

// outageTransit fails every unwrap with err, as a key service that cannot be reached or refuses this
// process does.
type outageTransit struct {
	*fakeTransit
	err error
}

func (o outageTransit) Unwrap(context.Context, []byte, map[string]string) ([]byte, error) {
	return nil, o.err
}

// A key service outage or a revoked access during an unwrap is reported as that, not as a wrap that
// does not unwrap (moved, forged, corrupted or retired); a service that refused the wrap itself keeps
// that wording. The cause stays visible to errors.Is either way.
func TestManager_UnwrapOutageIsNotReportedAsACorruptWrap(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	a := newFakeTransit("a")
	seeded := subjectkeytest.Manager(pool, a)
	if _, _, err := seeded.Current(t.Context(), "in-a", subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		cause   error
		wantMsg string
	}{
		"unreachable":    {fmt.Errorf("vault: %w", secretstore.ErrUnavailable), "could not unwrap"},
		"access revoked": {fmt.Errorf("vault: %w", kek.ErrAccess), "could not unwrap"},
		"wrap refused":   {fmt.Errorf("vault: %w", kek.ErrRefused), "does not unwrap"},
	} {
		m := subjectkeytest.Manager(pool, outageTransit{a, tc.cause})
		_, err := m.Key(t.Context(), "in-a", subjectkey.PurposeCred, 1)
		if !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%s: Key = %v, want the cause kept and %q in the text", name, err, tc.wantMsg)
		}
		if name != "wrap refused" && strings.Contains(err.Error(), "does not unwrap") {
			t.Errorf("%s: an outage reads as a corrupt wrap: %v", name, err)
		}
	}
}

// A person whose groups are assigned to two domains is refused by name, and
// nothing is written.
func TestManager_AmbiguousMembershipRefusesTheNewGeneration(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	def, a, b := newFakeTransit("default"), newFakeTransit("a"), newFakeTransit("b")
	m, svc := domains(t, pool, def, a, b)
	if err := svc.RecordLoginGroups(t.Context(), "gina", []string{"eng", "ops"}, false); err != nil {
		t.Fatal(err)
	}
	for g, d := range map[string]string{"eng": "a", "ops": "b"} {
		if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: g, Domain: d, SetBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := m.Current(t.Context(), "gina", subjectkey.PurposeCred)
	if !errors.Is(err, keydomain.ErrAmbiguous) {
		t.Fatalf("Current = %v, want ErrAmbiguous", err)
	}
	if got := generations(t, pool, "gina"); len(got) != 0 {
		t.Fatalf("a refused generation left rows: %+v", got)
	}
}

// Verify is the boot check: an undeclared domain, and a kek_id the domain no
// longer reaches, are refused with the row counts and a remedy that never
// deletes rows; destroyed generations hold no key and are not counted.
func TestVerify(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	def, a, b := newFakeTransit("default"), newFakeTransit("a"), newFakeTransit("b")
	m, svc := domains(t, pool, def, a, b)
	for owner, domain := range map[string]string{"p1": "a", "p2": "a", "p3": "b"} {
		if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: owner, Domain: domain, SetBy: "admin"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.Current(t.Context(), owner, subjectkey.PurposeCred); err != nil {
			t.Fatal(err)
		}
	}
	reach := func(d, id string) (kek.KEK, error) {
		for name, k := range map[string]*fakeTransit{keydomain.Default: def, "a": a, "b": b} {
			if name == d && k.id == id {
				return k, nil
			}
		}
		return nil, fmt.Errorf("domain %q no longer reaches %q", d, id)
	}
	if err := subjectkey.Verify(t.Context(), pool, []string{"a", "b"}, reach); err != nil {
		t.Fatalf("a consistent database refused: %v", err)
	}

	err := subjectkey.Verify(t.Context(), pool, []string{"b"}, reach)
	if err == nil || !strings.Contains(err.Error(), `2 live principal keys name key domain "a"`) ||
		!strings.Contains(err.Error(), "Keep the domain declared") || !strings.Contains(err.Error(), "Never delete the rows") {
		t.Fatalf("undeclared domain a = %v; want its live-row count and the remedy", err)
	}
	if err := subjectkey.Verify(t.Context(), pool, nil, nil); err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("no domain declared, no reach check = %v; want both named", err)
	}

	moved := func(d, id string) (kek.KEK, error) {
		if d == "b" {
			return nil, errors.New("b now names another key")
		}
		return reach(d, id)
	}
	if err := subjectkey.Verify(t.Context(), pool, []string{"a", "b"}, moved); err == nil || !strings.Contains(err.Error(), `1 live principal keys in domain "b"`) {
		t.Fatalf("a domain whose key moved = %v; want the row count of b", err)
	}
	// Declared-only (the rewrap's check) does not ask whether a key is reached.
	if err := subjectkey.Verify(t.Context(), pool, []string{"a", "b"}, nil); err != nil {
		t.Fatalf("declared-only check: %v", err)
	}

	// Erasing the subjects clears the refusal: a tombstone holds no key.
	for _, owner := range []string{"p1", "p2"} {
		if _, err := m.Destroy(t.Context(), owner, subjectkey.PurposeCred); err != nil {
			t.Fatal(err)
		}
	}
	if err := subjectkey.Verify(t.Context(), pool, []string{"b"}, reach); err != nil {
		t.Fatalf("after destroying domain a's subjects, with a removed: %v", err)
	}
}

// Erasing a person's credentials destroys their credential key only. Their
// audit-seal key stays live, so the domain is still refused, and the remedy
// names the act that destroys each key instead of offering credential erasure
// for both: it leads with keeping the domain, and says the audit erasure is
// irreversible and a retention decision.
func TestVerify_AuditSealKeyOutlivesCredentialErasure(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	def, a, b := newFakeTransit("default"), newFakeTransit("a"), newFakeTransit("b")
	m, svc := domains(t, pool, def, a, b)
	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "user", Subject: "p1", Domain: "a", SetBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{subjectkey.PurposeCred, subjectkey.PurposeAuditSeal} {
		if _, _, err := m.Current(t.Context(), "p1", purpose); err != nil {
			t.Fatal(err)
		}
	}
	// What DELETE /people/{principal}/credentials destroys.
	if _, err := m.Destroy(t.Context(), "p1", subjectkey.PurposeCred); err != nil {
		t.Fatal(err)
	}
	err := subjectkey.Verify(t.Context(), pool, []string{"b"}, nil)
	if err == nil || !strings.Contains(err.Error(), `1 live principal keys name key domain "a"`) {
		t.Fatalf("after credential erasure, with a removed = %v; want the audit-seal key still counted", err)
	}
	remedy := err.Error()[strings.Index(err.Error(), "does not declare. ")+len("does not declare. "):]
	if !strings.HasPrefix(remedy, "Keep the domain declared") {
		t.Errorf("the remedy does not lead with keeping the domain: %q", remedy)
	}
	for _, want := range []string{"leaves their audit-seal key live", "audit_personal_fields", "irreversible", "retention", "Never delete the rows"} {
		if !strings.Contains(remedy, want) {
			t.Errorf("the remedy does not say %q: %q", want, remedy)
		}
	}
	if strings.Contains(remedy, "erase or destroy those subjects") {
		t.Errorf("the remedy still offers credential erasure for every key: %q", remedy)
	}
	// Only the audit-seal key's own destruction clears the refusal.
	if _, err := m.Destroy(t.Context(), "p1", subjectkey.PurposeAuditSeal); err != nil {
		t.Fatal(err)
	}
	if err := subjectkey.Verify(t.Context(), pool, []string{"b"}, nil); err != nil {
		t.Fatalf("after both keys are destroyed, with a removed: %v", err)
	}
}

// Verify on a database the release has not migrated says so, rather than reading as a Postgres
// outage: -rotate-age-key and -rewrap run it without migrating.
func TestVerify_UnmigratedDatabaseNamesTheRemedy(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	if _, err := pool.Exec(t.Context(), `DROP TABLE principal_keys CASCADE`); err != nil {
		t.Fatal(err)
	}
	err := subjectkey.Verify(t.Context(), pool, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "migrate-only") {
		t.Fatalf("Verify on an unmigrated database = %v, want an error naming -migrate-only", err)
	}
	if errors.Is(err, secretstore.ErrUnavailable) {
		t.Errorf("Verify reported an outage for a missing table: %v", err)
	}
}
