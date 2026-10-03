// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// pkRow is one live principal_keys row as Rewrap selects it.
type pkRow struct {
	owner, purpose string
	version        int
	domain, kekID  string
	wrapped        []byte
}

// update is one UPDATE principal_keys Rewrap issued: owner, purpose, version,
// the new kek_id and the new wrapped key.
type update struct {
	owner, purpose string
	version        int
	kekID          string
	wrapped        []byte
}

// fakeTx is the two calls Rewrap makes on its transaction.
type fakeTx struct {
	pgx.Tx
	rows     []pkRow
	queryErr error
	scanErr  error
	execErr  error
	updates  []update
}

func (f *fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &fakeRows{rows: f.rows, scanErr: f.scanErr, i: -1}, nil
}

func (f *fakeTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	f.updates = append(f.updates, update{
		owner: args[0].(string), purpose: args[1].(string), version: args[2].(int),
		kekID: args[3].(string), wrapped: args[4].([]byte),
	})
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

type fakeRows struct {
	pgx.Rows
	rows    []pkRow
	scanErr error
	i       int
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Next() bool { r.i++; return r.i < len(r.rows) }

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.rows[r.i]
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*int) = row.owner, row.purpose, row.version
	*dest[3].(*string), *dest[4].(*string), *dest[5].(*[]byte) = row.domain, row.kekID, row.wrapped
	return nil
}

// verKEK is a local KEK whose wraps name a key version, so a row can be
// behind the latest.
type verKEK struct {
	*kek.Local
	version string
}

func (v verKEK) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	w, err := v.Local.Wrap(ctx, dek, bind)
	return append([]byte(v.version+"|"), w...), err
}

func (v verKEK) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	_, inner, _ := bytes.Cut(wrapped, []byte("|"))
	return v.Local.Unwrap(ctx, inner, bind)
}

func (verKEK) LatestVersion(context.Context) (string, error) { return "", nil }

func (verKEK) WrapVersion(wrapped []byte) (string, error) {
	v, _, ok := bytes.Cut(wrapped, []byte("|"))
	if !ok {
		return "", errors.New("unversioned wrap")
	}
	return string(v), nil
}

func localKEK(t *testing.T) *kek.Local {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sealedRow(t *testing.T, k kek.KEK, owner, purpose string, version int, key byte) pkRow {
	t.Helper()
	w, err := k.Wrap(context.Background(), bytes.Repeat([]byte{key}, kek.DEKSize), kek.PrincipalBind(owner, purpose, version, DomainDefault))
	if err != nil {
		t.Fatal(err)
	}
	return pkRow{owner: owner, purpose: purpose, version: version, domain: DomainDefault, kekID: k.ID(), wrapped: w}
}

func only(k kek.KEK) func(string, string) (kek.KEK, error) {
	return func(string, string) (kek.KEK, error) { return k, nil }
}

func TestRewrapMovesEveryRowOntoTheTargetAndKeepsEachKeyAndBinding(t *testing.T) {
	from, to := localKEK(t), localKEK(t)
	tx := &fakeTx{rows: []pkRow{
		sealedRow(t, from, "alice", PurposeCred, 1, 0x11),
		sealedRow(t, from, "bob", PurposeAuditSeal, 3, 0x22),
	}}

	n, err := Rewrap(context.Background(), tx, only(from), only(to), nil, nil)
	if err != nil || n != 2 || len(tx.updates) != 2 {
		t.Fatalf("Rewrap = %d, %v with %d updates; want 2 rows moved", n, err, len(tx.updates))
	}
	for i, u := range tx.updates {
		src := tx.rows[i]
		if u.owner != src.owner || u.purpose != src.purpose || u.version != src.version || u.kekID != to.ID() {
			t.Errorf("update %d = %+v; want the same generation now under %s", i, u, to.ID())
		}
		key, err := to.Unwrap(context.Background(), u.wrapped, kek.PrincipalBind(src.owner, src.purpose, src.version, DomainDefault))
		if err != nil || !bytes.Equal(key, bytes.Repeat([]byte{[]byte{0x11, 0x22}[i]}, kek.DEKSize)) {
			t.Errorf("update %d: the new wrap does not open to the original key under the same binding: %v", i, err)
		}
		if _, err := from.Unwrap(context.Background(), u.wrapped, kek.PrincipalBind(src.owner, src.purpose, src.version, DomainDefault)); err == nil {
			t.Errorf("update %d: the old KEK still opens the new wrap", i)
		}
	}
}

func TestRewrapLeavesAloneWhatIsAlreadyAtTheTarget(t *testing.T) {
	to := localKEK(t)
	other := localKEK(t)
	tx := &fakeTx{rows: []pkRow{
		sealedRow(t, to, "alice", PurposeCred, 1, 0x11),  // already under the target
		sealedRow(t, other, "bob", PurposeCred, 1, 0x22), // target says nil: not this operation's
	}}
	target := func(_, kekID string) (kek.KEK, error) {
		if kekID == other.ID() {
			return nil, nil
		}
		return to, nil
	}
	n, err := Rewrap(context.Background(), tx, only(other), target, nil, nil)
	if err != nil || n != 0 || len(tx.updates) != 0 {
		t.Fatalf("Rewrap = %d, %v, %d updates; want nothing moved", n, err, len(tx.updates))
	}
}

func TestRewrapMovesARowBehindTheLatestVersionAndReportsTheRotation(t *testing.T) {
	base := localKEK(t)
	old, latest := verKEK{base, "v1"}, verKEK{base, "v2"}
	tx := &fakeTx{rows: []pkRow{
		sealedRow(t, old, "alice", PurposeCred, 1, 0x11),    // behind
		sealedRow(t, latest, "alice", PurposeCred, 2, 0x22), // at latest
	}}
	rotated := map[string]bool{}

	n, err := Rewrap(context.Background(), tx, only(old), only(latest), map[string]string{base.ID(): "v2"}, rotated)
	if err != nil || n != 1 {
		t.Fatalf("Rewrap = %d, %v; want exactly the row behind moved", n, err)
	}
	if len(tx.updates) != 1 || tx.updates[0].version != 1 {
		t.Fatalf("updates = %+v; want generation 1 only", tx.updates)
	}
	if rotated[base.ID()] {
		t.Error("the row now at the latest version was reported as rotated onto another version")
	}

	// Moved onto a version other than latest: rotated names the kek_id.
	tx = &fakeTx{rows: []pkRow{sealedRow(t, old, "alice", PurposeCred, 1, 0x11)}}
	rotated = map[string]bool{}
	if _, err := Rewrap(context.Background(), tx, only(old), only(verKEK{base, "v1"}), map[string]string{base.ID(): "v9"}, rotated); err != nil {
		t.Fatal(err)
	}
	if !rotated[base.ID()] {
		t.Error("a wrap made under a version other than latest was not reported")
	}
}

func TestRewrapStopsAtTheFirstFailureAndNamesTheRow(t *testing.T) {
	from, to, stranger := localKEK(t), localKEK(t), localKEK(t)
	boom := errors.New("boom")
	row := func() []pkRow { return []pkRow{sealedRow(t, from, "alice", PurposeCred, 4, 0x11)} }

	for _, tc := range []struct {
		name   string
		tx     *fakeTx
		source func(string, string) (kek.KEK, error)
		target func(string, string) (kek.KEK, error)
		want   string
	}{
		{"select", &fakeTx{queryErr: boom}, only(from), only(to), "principal_keys select"},
		{"scan", &fakeTx{rows: row(), scanErr: boom}, only(from), only(to), "principal_keys scan"},
		{"target resolution", &fakeTx{rows: row()}, only(from), func(string, string) (kek.KEK, error) { return nil, boom }, `owner="alice", purpose="cred", version=4`},
		{"source resolution", &fakeTx{rows: row()}, func(string, string) (kek.KEK, error) { return nil, boom }, only(to), "principal key"},
		{"unwrap with the old key", &fakeTx{rows: row()}, only(stranger), only(to), "unwrap with the old key"},
		{"update", &fakeTx{rows: row(), execErr: boom}, only(from), only(to), "update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Rewrap(context.Background(), tc.tx, tc.source, tc.target, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || n != 0 {
				t.Fatalf("Rewrap = %d, %v; want a failure containing %q and no count", n, err, tc.want)
			}
			if len(tc.tx.updates) != 0 {
				t.Fatalf("a failed rewrap issued updates: %+v", tc.tx.updates)
			}
		})
	}
}

func TestRewrapRefusesAWrapWhoseVersionCannotBeRead(t *testing.T) {
	base := localKEK(t)
	ver := verKEK{base, "v1"}
	// A row at the target's own id whose wrap carries no version: Behind errors.
	bare := sealedRow(t, base, "alice", PurposeCred, 1, 0x11)
	bare.kekID = ver.ID()
	tx := &fakeTx{rows: []pkRow{bare}}
	_, err := Rewrap(context.Background(), tx, only(ver), only(ver), map[string]string{base.ID(): "v2"}, nil)
	if err == nil || !strings.Contains(fmt.Sprint(err), "unversioned wrap") {
		t.Fatalf("Rewrap = %v; want the unreadable version refused", err)
	}
}
