// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// miscCovClosedPool is a pool that has been closed: every statement against it fails at once, with no
// network and no timing. closedErr is the error the pool answers with, so a test can prove a store
// error was wrapped (errors.Is) and not replaced.
func miscCovClosedPool(t *testing.T) (pool *pgxpool.Pool, closedErr error) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	pool.Close()
	closedErr = pool.Ping(t.Context())
	if closedErr == nil {
		t.Fatal("a closed pool answered a ping")
	}
	return pool, closedErr
}

// miscCovLogs records every slog record the default logger takes while a test runs, and announces each
// message on seen so a test can wait on one without sleeping.
type miscCovLogs struct {
	mu   sync.Mutex
	recs []slog.Record
	seen chan string
}

func (l *miscCovLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *miscCovLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *miscCovLogs) WithGroup(string) slog.Handler            { return l }
func (l *miscCovLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	select {
	case l.seen <- r.Message:
	default:
	}
	return nil
}

// matching returns, under the lock, every record whose message contains text.
func (l *miscCovLogs) matching(text string) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range l.recs {
		if strings.Contains(r.Message, text) {
			out = append(out, r)
		}
	}
	return out
}

// find returns the first record whose message contains text.
func (l *miscCovLogs) find(text string) (slog.Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if strings.Contains(r.Message, text) {
			return r, true
		}
	}
	return slog.Record{}, false
}

func (l *miscCovLogs) attr(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, ok = a.Value, true
			return false
		}
		return true
	})
	return v, ok
}

// await blocks until a message containing text has been logged, and fails the test after a generous bound.
func (l *miscCovLogs) await(t *testing.T, text string) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		if _, ok := l.find(text); ok {
			return
		}
		select {
		case <-l.seen:
		case <-timeout:
			t.Fatalf("no log message containing %q within 30s", text)
		}
	}
}

func miscCovCaptureLogs(t *testing.T) *miscCovLogs {
	t.Helper()
	l := &miscCovLogs{seen: make(chan string, 64)}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// --- session revocations -------------------------------------------------------------------------

func TestMiscCovSessionRevocationsForMountsOnlyWithOIDC(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	if got := sessionRevocationsFor(nil, pool); got != nil {
		t.Fatalf("with OIDC unconfigured the revoke surface was wired anyway: %T", got)
	}
	got := sessionRevocationsFor(&oidc.Authenticator{}, pool)
	pr, ok := got.(*pgSessionRevocations)
	if !ok || pr.pool != pool {
		t.Fatalf("with OIDC configured got %T, want a *pgSessionRevocations over the given pool", got)
	}
}

func TestMiscCovSessionRevocationsAppNow(t *testing.T) {
	fixed := time.Now().UTC().Truncate(time.Second)
	if got := (&pgSessionRevocations{now: func() time.Time { return fixed }}).appNow(); !got.Equal(fixed) {
		t.Fatalf("appNow with an injected clock = %v, want %v", got, fixed)
	}
	r := &pgSessionRevocations{}
	first := r.appNow()
	if first.IsZero() || r.appNow().Before(first) {
		t.Fatalf("appNow with no injected clock gave %v then an earlier or zero time", first)
	}
}

// Every statement of the revocations store wraps the store's own error under a message naming the
// operation, so an operator reading the log can tell a failed revoke from a failed check.
func TestMiscCovSessionRevocationsWrapStoreErrors(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	r := &pgSessionRevocations{pool: pool}
	ctx := t.Context()
	issued := time.Now().UTC().Truncate(time.Second)

	calls := []struct {
		name string
		want string
		do   func() error
	}{
		{"IsSessionRevoked", "wardynd: is-session-revoked query", func() error { _, err := r.IsSessionRevoked(ctx, "sub", "a@example.com", issued); return err }},
		{"SessionStatus without epoch", "wardynd: is-session-revoked query", func() error { _, err := r.SessionStatus(ctx, "sub", "a@example.com", issued, -1); return err }},
		{"SessionStatus with epoch", "wardynd: is-session-revoked query", func() error { _, err := r.SessionStatus(ctx, "sub", "a@example.com", issued, 4); return err }},
		{"RevokeSub", "wardynd: revoke session cutoff", func() error { return r.RevokeSub(ctx, "sub") }},
		{"RevokeAll", "wardynd: revoke session cutoff", func() error { return r.RevokeAll(ctx) }},
		{"CutSessions", "wardynd: cut sessions", func() error { return r.CutSessions(ctx, "sub") }},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			err := c.do()
			if err == nil {
				t.Fatal("no error from a closed pool")
			}
			if !errors.Is(err, closedErr) {
				t.Errorf("err = %v, want it to wrap the pool's error %v", err, closedErr)
			}
			if !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("err = %q, want prefix %q", err.Error(), c.want)
			}
		})
	}

	// A failed check never reports a session as revoked or deactivated: the caller gets the error and the
	// zero (live) status, and decides what an unanswered question means.
	if revoked, _ := r.IsSessionRevoked(ctx, "sub", "", issued); revoked {
		t.Error("IsSessionRevoked reported revoked for a query that failed")
	}
	if st, _ := r.SessionStatus(ctx, "sub", "", issued, 0); st != oidc.SessionLive {
		t.Errorf("SessionStatus for a failed query = %v, want SessionLive", st)
	}
}

// --- audit seal ----------------------------------------------------------------------------------

func TestMiscCovSealModeOfIsTheValidatedMode(t *testing.T) {
	for in, want := range map[string]audit.SealMode{"off": audit.SealOff, "fields": audit.SealFields, "full": audit.SealFull} {
		if got := sealModeOf(&bootFlags{auditSeal: &in}); got != want {
			t.Errorf("WARDYN_AUDIT_SEAL=%q: mode = %q, want %q", in, got, want)
		}
	}
}

func TestMiscCovUnsealerIsTheArmedSealerOrNil(t *testing.T) {
	var nilSrc *auditSealSource
	if got := nilSrc.unsealer(); got != nil {
		t.Errorf("a nil source gave %T, want nil", got)
	}
	src := newAuditSealSource(audit.SealFields)
	if got := src.unsealer(); got != nil {
		t.Errorf("an unarmed source gave %T, want nil (a typed-nil Sealer would read as a live one)", got)
	}
	sealer := &audit.Sealer{}
	src.arm(sealer)
	if got := src.unsealer(); got != audit.Unsealer(sealer) {
		t.Errorf("an armed source gave %v, want the armed sealer", got)
	}
	nilSrc.arm(sealer) // arming a nil source is a no-op, not a panic
}

// A Sealer that cannot seal under a subject key and holds no pending key refuses the row; the recorder
// returns that refusal, names the action, and writes nothing below.
func TestMiscCovSealingRecorderRefusesWhenTheSealerFails(t *testing.T) {
	inner := &captureRecorder{}
	src := newAuditSealSource(audit.SealFields)
	src.arm(&audit.Sealer{Keys: downKeys{}}) // no Pending: nowhere to hold the row
	err := sealingRecorder{inner: inner, src: src}.Record(t.Context(), decideRow("words"))
	if err == nil || !strings.HasPrefix(err.Error(), "audit seal: approval.decide not recorded: ") {
		t.Fatalf("err = %v, want the refusal naming approval.decide", err)
	}
	if len(inner.evs) != 0 {
		t.Fatalf("a row the Sealer refused reached the recorder below: %d rows", len(inner.evs))
	}
}

// A pending row, with a spool to hold it, goes to the spool and not to the recorder below, and what the
// spool holds is sealed (the plaintext never lands on disk).
func TestMiscCovSealingRecorderHoldsAPendingRowInTheSpool(t *testing.T) {
	spoolPath := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	spool, err := api.NewAuditSpool(spoolPath)
	if err != nil {
		t.Fatalf("open the spool: %v", err)
	}
	inner := &captureRecorder{}
	src := newAuditSealSource(audit.SealFields)
	pending := make([]byte, 32)
	src.arm(&audit.Sealer{Keys: downKeys{}, Pending: func() []byte { return pending }})

	logs := miscCovCaptureLogs(t)
	if err := (sealingRecorder{inner: inner, src: src, spool: spool}).Record(t.Context(), decideRow("a private reason")); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(inner.evs) != 0 {
		t.Errorf("a pending row reached the recorder below: %d rows", len(inner.evs))
	}
	if spool.Lines() != 1 {
		t.Fatalf("spool holds %d rows, want 1", spool.Lines())
	}
	if _, ok := logs.find("held in the audit spool"); !ok {
		t.Error("holding a row under the pending key was not logged")
	}
	raw, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "a private reason") {
		t.Error("the spool holds the reason in the clear")
	}
	if !strings.Contains(string(raw), audit.PendingMarker) {
		t.Errorf("the spooled row is not marked %q", audit.PendingMarker)
	}
}

// The adapter hands the subject key manager's answers through unchanged: an owner the manager refuses is
// refused here, and a key store that cannot answer is not mistaken for an erased key.
func TestMiscCovSubjectSealKeysPassTheManagersRefusalThrough(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	k := subjectSealKeys{m: subjectkey.New(pool, subjectkey.Resolver{})}
	if _, _, err := k.Current(t.Context(), "", audit.SealPurpose); !errors.Is(err, subjectkey.ErrOperatorOwner) {
		t.Errorf("Current for the operator namespace = %v, want ErrOperatorOwner", err)
	}
	_, err := k.Key(t.Context(), audit.SealPurpose, uuid.New())
	if !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, audit.ErrKeyErased) {
		t.Errorf("Key over a closed pool = %v, want ErrUnavailable and not ErrKeyErased", err)
	}
}

// miscCovMemKeys is a secretKeyStore over a map that counts its writes.
type miscCovMemKeys struct {
	vals    map[string][]byte
	getErr  error
	putErr  error
	puts    int
	putName string
}

func (m *miscCovMemKeys) Get(_ context.Context, name string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	v, ok := m.vals[name]
	if !ok {
		return nil, secretstore.ErrNotFound
	}
	return v, nil
}

func (m *miscCovMemKeys) Put(_ context.Context, name string, v []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.puts++
	m.putName = name
	if m.vals == nil {
		m.vals = map[string][]byte{}
	}
	m.vals[name] = v
	return nil
}

func miscCovBootKeys(m *miscCovMemKeys) bootKeyStore {
	return bootKeyStore{secretKeyStore: m, lockCreate: holdsSingleInstanceLock}
}

func TestMiscCovLoadAuditPendingKey(t *testing.T) {
	valid := make([]byte, 32)
	valid[0] = 9
	boom := errors.New("secret store down")

	t.Run("read-only with no key creates nothing", func(t *testing.T) {
		m := &miscCovMemKeys{}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), false)
		if err != nil || got != nil || m.puts != 0 {
			t.Fatalf("got %v, %v, %d puts; want nil, nil and no write", got, err, m.puts)
		}
	})
	t.Run("read-only returns the stored key", func(t *testing.T) {
		m := &miscCovMemKeys{vals: map[string][]byte{secretAuditPendingKey: valid}}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), false)
		if err != nil || string(got) != string(valid) {
			t.Fatalf("got %v, %v; want the stored key", got, err)
		}
	})
	t.Run("read-only ignores a key of the wrong size", func(t *testing.T) {
		m := &miscCovMemKeys{vals: map[string][]byte{secretAuditPendingKey: make([]byte, 16)}}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), false)
		if err != nil || got != nil || m.puts != 0 {
			t.Fatalf("got %v, %v, %d puts; want nil, nil and no write", got, err, m.puts)
		}
	})
	t.Run("read-only fails closed on a store error", func(t *testing.T) {
		m := &miscCovMemKeys{getErr: boom}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), false)
		if !errors.Is(err, boom) || got != nil || m.puts != 0 {
			t.Fatalf("got %v, %v, %d puts; want the store's error and no write", got, err, m.puts)
		}
	})
	t.Run("create makes and persists a 32-byte key", func(t *testing.T) {
		m := &miscCovMemKeys{}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), true)
		if err != nil || len(got) != 32 {
			t.Fatalf("got %d bytes, %v; want a 32-byte key", len(got), err)
		}
		if m.puts != 1 || m.putName != secretAuditPendingKey || string(m.vals[secretAuditPendingKey]) != string(got) {
			t.Fatalf("persisted %d times under %q; want the returned key stored once under %q", m.puts, m.putName, secretAuditPendingKey)
		}
	})
	t.Run("create keeps the key that exists", func(t *testing.T) {
		m := &miscCovMemKeys{vals: map[string][]byte{secretAuditPendingKey: valid}}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), true)
		if err != nil || string(got) != string(valid) || m.puts != 0 {
			t.Fatalf("got %v, %v, %d puts; want the stored key and no write", got, err, m.puts)
		}
	})
	t.Run("create reports a failed persist", func(t *testing.T) {
		m := &miscCovMemKeys{putErr: boom}
		got, err := loadAuditPendingKey(t.Context(), miscCovBootKeys(m), true)
		if !errors.Is(err, boom) || got != nil {
			t.Fatalf("got %v, %v; want the store's error and no key", got, err)
		}
	})
}

// miscCovKeyedStore is a secret store that exposes a per-subject key manager, as the Postgres one does.
type miscCovKeyedStore struct {
	secretstore.Store
	m *subjectkey.Manager
}

func (k miscCovKeyedStore) SubjectKeys() *subjectkey.Manager { return k.m }

func TestMiscCovArmAuditSealRefusesWithoutSubjectKeys(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	m := &miscCovMemKeys{}
	off := newAuditSealSource(audit.SealOff)
	if err := armAuditSeal(t.Context(), off, pool, nil, miscCovBootKeys(m)); err != nil || off.unsealer() != nil {
		t.Fatalf("sealing off with no subject keys: err %v, armed %v; want to start unarmed", err, off.unsealer() != nil)
	}
	for _, mode := range []audit.SealMode{audit.SealFields, audit.SealFull} {
		src := newAuditSealSource(mode)
		err := armAuditSeal(t.Context(), src, pool, nil, miscCovBootKeys(m))
		if err == nil || !strings.Contains(err.Error(), "no per-subject keys") {
			t.Errorf("mode %q with no subject keys: err = %v, want a refusal to start", mode, err)
		}
		if src.unsealer() != nil {
			t.Errorf("mode %q: a source with no keys was armed", mode)
		}
	}
	if m.puts != 0 {
		t.Errorf("a refused arm wrote %d secrets", m.puts)
	}
}

func TestMiscCovArmAuditSealArmsTheSealer(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	secrets := miscCovKeyedStore{m: subjectkey.New(pool, subjectkey.Resolver{})}

	stored := make([]byte, 32)
	stored[0] = 7
	for _, tc := range []struct {
		name         string
		mode         audit.SealMode
		vals         map[string][]byte
		wantActor    bool
		wantPending  []byte // nil: the sealer holds no pending key
		wantPutsKeys int
	}{
		{"fields creates the pending key", audit.SealFields, nil, false, nil, 1},
		{"full seals the actor", audit.SealFull, map[string][]byte{secretAuditPendingKey: stored}, true, stored, 0},
		{"off still loads an existing pending key", audit.SealOff, map[string][]byte{secretAuditPendingKey: stored}, false, stored, 0},
		{"off with none holds none and creates none", audit.SealOff, nil, false, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &miscCovMemKeys{vals: tc.vals}
			src := newAuditSealSource(tc.mode)
			if err := armAuditSeal(t.Context(), src, pool, secrets, miscCovBootKeys(mem)); err != nil {
				t.Fatalf("armAuditSeal: %v", err)
			}
			sealer := src.sealer.Load()
			if sealer == nil {
				t.Fatal("the source was not armed")
			}
			if sealer.SealActor != tc.wantActor {
				t.Errorf("SealActor = %v, want %v", sealer.SealActor, tc.wantActor)
			}
			got := sealer.Pending()
			switch {
			case tc.wantPending != nil && string(got) != string(tc.wantPending):
				t.Errorf("pending key = %v, want the stored one", got)
			case tc.wantPending == nil && tc.wantPutsKeys == 0 && got != nil:
				t.Errorf("pending key = %v, want none", got)
			case tc.wantPutsKeys == 1 && (len(got) != 32 || string(got) != string(mem.vals[secretAuditPendingKey])):
				t.Errorf("pending key = %v, want the one persisted", got)
			}
			if mem.puts != tc.wantPutsKeys {
				t.Errorf("pending key written %d times, want %d", mem.puts, tc.wantPutsKeys)
			}
		})
	}

	// The Sealer's directory lookups go to the store and surface its errors, rather than reading a
	// failed lookup as "not erased".
	mem := &miscCovMemKeys{}
	src := newAuditSealSource(audit.SealFields)
	if err := armAuditSeal(t.Context(), src, pool, secrets, miscCovBootKeys(mem)); err != nil {
		t.Fatal(err)
	}
	sealer := src.sealer.Load()
	if _, err := sealer.Resolve(t.Context(), "someone@example.com"); !errors.Is(err, closedErr) {
		t.Errorf("Resolve on a failing store = %v, want the store's error", err)
	}
	if gone, err := sealer.GoneSince(t.Context(), "someone", time.Time{}); !errors.Is(err, closedErr) || gone {
		t.Errorf("GoneSince on a failing store = %v, %v; want false and the store's error", gone, err)
	}
	if _, ok := sealer.Subjects.(store.PG); !ok {
		t.Errorf("Subjects = %T, want the store the pool backs", sealer.Subjects)
	}
}

func TestMiscCovArmAuditSealReportsAPendingKeyThatCannotBeLoaded(t *testing.T) {
	pool, _ := miscCovClosedPool(t)
	secrets := miscCovKeyedStore{m: subjectkey.New(pool, subjectkey.Resolver{})}
	boom := errors.New("secret store down")
	src := newAuditSealSource(audit.SealFields)
	err := armAuditSeal(t.Context(), src, pool, secrets, miscCovBootKeys(&miscCovMemKeys{getErr: boom}))
	if !errors.Is(err, boom) || src.unsealer() != nil {
		t.Fatalf("err = %v, armed %v; want the store's error and an unarmed source", err, src.unsealer() != nil)
	}
}

// armSubjectKeyed stops at the first refusal and arms nothing: with no subject keys the masking manifests
// refuse, and with a masking registry that cannot be read the same.
func TestMiscCovArmSubjectKeyedRefusesBeforeArmingTheAuditSeal(t *testing.T) {
	pool, closedErr := miscCovClosedPool(t)
	mem := &miscCovMemKeys{}
	for name, tc := range map[string]struct {
		secrets secretstore.Store
		want    string
	}{
		"no subject keys":     {nil, "no per-subject keys"},
		"registry unreadable": {miscCovKeyedStore{m: subjectkey.New(pool, subjectkey.Resolver{})}, "masking registry could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			src := newAuditSealSource(audit.SealFields)
			m, st, err := armSubjectKeyed(t.Context(), pool, tc.secrets, secretmask.NewRegistry(), &maskScope{}, src, miscCovBootKeys(mem))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if tc.secrets != nil && !errors.Is(err, closedErr) {
				t.Errorf("err = %v, want it to wrap the store's error", err)
			}
			if m != nil || st != nil {
				t.Errorf("returned manifests %v / store %v alongside an error", m, st)
			}
			if src.unsealer() != nil {
				t.Error("the audit seal was armed after the masking manifests refused")
			}
		})
	}
	if mem.puts != 0 {
		t.Errorf("a refused arm wrote %d secrets", mem.puts)
	}
}

// --- audit retention -----------------------------------------------------------------------------

// miscCovRetention is a store.AuditRetention that answers a policy write and signals each ensure.
type miscCovRetention struct {
	store.AuditRetention
	policyDays []int
	change     store.AuditRetentionPolicyChange
	policyErr  error

	ensured    chan struct{}
	ensureErr  error
	autodrops  int
	autodropEr error
}

func (r *miscCovRetention) SetAuditRetentionPolicy(_ context.Context, days int) (store.AuditRetentionPolicyChange, error) {
	r.policyDays = append(r.policyDays, days)
	return r.change, r.policyErr
}

func (r *miscCovRetention) EnsureAuditPartitions(context.Context, int) (int, error) {
	if r.ensured != nil {
		r.ensured <- struct{}{}
	}
	return 0, r.ensureErr
}

func (r *miscCovRetention) AutodropAuditPartition(context.Context) (store.AuditRetentionDrop, bool, error) {
	r.autodrops++
	return store.AuditRetentionDrop{}, false, r.autodropEr
}

func miscCovChange(outcome string, effective int, pendingDays *int, pendingAt *time.Time) store.AuditRetentionPolicyChange {
	return store.AuditRetentionPolicyChange{Outcome: outcome, AuditRetentionPolicy: types.AuditRetentionPolicy{
		EffectiveDays: effective, PendingDays: pendingDays, PendingEffectiveAt: pendingAt}}
}

func TestMiscCovRecordAuditRetentionPolicy(t *testing.T) {
	pendingDays := 30
	pendingAt := time.Now().UTC().Truncate(time.Second).AddDate(0, 1, 0)
	boom := errors.New("policy write refused")

	for _, tc := range []struct {
		name      string
		rs        *miscCovRetention
		wantMsg   string
		wantLevel slog.Level
		wantAttr  string
	}{
		{"unchanged is informational", &miscCovRetention{change: miscCovChange("unchanged", 90, nil, nil)},
			"audit retention policy", slog.LevelInfo, ""},
		{"a change is a warning with the pending window", &miscCovRetention{change: miscCovChange("pending", 90, &pendingDays, &pendingAt)},
			"audit retention policy changed", slog.LevelWarn, "pending_days"},
		{"a change with no pending window omits it", &miscCovRetention{change: miscCovChange("applied", 365, nil, nil)},
			"audit retention policy changed", slog.LevelWarn, ""},
		{"a failed write is an error and the stored policy stands", &miscCovRetention{policyErr: boom},
			"audit retention policy NOT recorded", slog.LevelError, "err"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := miscCovCaptureLogs(t)
			recordAuditRetentionPolicy(t.Context(), tc.rs, 90)
			if len(tc.rs.policyDays) != 1 || tc.rs.policyDays[0] != 90 {
				t.Fatalf("policy written with %v, want exactly [90]", tc.rs.policyDays)
			}
			recs := logs.matching("audit retention policy")
			if len(recs) != 1 {
				t.Fatalf("logged %d retention-policy records, want 1", len(recs))
			}
			rec := recs[0]
			if !strings.Contains(rec.Message, tc.wantMsg) || rec.Level != tc.wantLevel {
				t.Fatalf("logged %q at %v, want %q at %v", rec.Message, rec.Level, tc.wantMsg, tc.wantLevel)
			}
			_, hasPending := logs.attr(rec, "pending_days")
			if hasPending != (tc.wantAttr == "pending_days") {
				t.Errorf("pending_days attribute present = %v, want %v", hasPending, tc.wantAttr == "pending_days")
			}
			if tc.wantAttr == "pending_days" {
				if v, _ := logs.attr(rec, "pending_days"); v.Int64() != 30 {
					t.Errorf("pending_days = %v, want 30", v)
				}
			}
			if tc.wantAttr == "err" {
				if v, ok := logs.attr(rec, "err"); !ok || !errors.Is(v.Any().(error), boom) {
					t.Errorf("err attribute = %v, want the store's error", v)
				}
			}
		})
	}
}

// The sweeper sweeps at once and then on every tick, and returns when its context ends.
func TestMiscCovRunAuditRetentionSweeperTicksUntilCancelled(t *testing.T) {
	rs := &miscCovRetention{ensured: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAuditRetentionSweeper(ctx, rs, false, time.Millisecond)
	}()
	for i := 0; i < 3; i++ { // the immediate pass and two ticks
		select {
		case <-rs.ensured:
		case <-time.After(30 * time.Second):
			t.Fatalf("sweep %d never ran", i+1)
		}
	}
	cancel()
	// A pass may be blocked sending its signal; drain until the sweeper has returned.
	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-done:
			if rs.autodrops != 0 {
				t.Errorf("a sweeper with autodrop off dropped %d times", rs.autodrops)
			}
			return
		case <-rs.ensured:
		case <-timeout:
			t.Fatal("the sweeper did not return after its context was cancelled")
		}
	}
}

// An autodrop that fails ends the pass, logs it, and tries nothing more in that pass.
func TestMiscCovSweepAuditRetentionStopsAtAFailedAutodrop(t *testing.T) {
	logs := miscCovCaptureLogs(t)
	boom := errors.New("drop refused")
	rs := &miscCovRetention{autodropEr: boom}
	sweepAuditRetention(t.Context(), rs, true)
	if rs.autodrops != 1 {
		t.Fatalf("autodrop asked %d times after a failure, want 1", rs.autodrops)
	}
	rec, ok := logs.find("audit autodrop failed")
	if !ok || rec.Level != slog.LevelWarn {
		t.Fatalf("failed autodrop logged = %v at %v, want a warning", ok, rec.Level)
	}
	if v, ok := logs.attr(rec, "err"); !ok || !errors.Is(v.Any().(error), boom) {
		t.Errorf("err attribute = %v, want the store's error", v)
	}
}

// Booting the retention sweep over a database that cannot be reached records the failure, warns when
// autodrop is on, and does not stop the boot or the sweeper.
func TestMiscCovStartAuditRetentionSurvivesAnUnreachableDatabase(t *testing.T) {
	for _, autodrop := range []bool{false, true} {
		name := "autodrop off"
		if autodrop {
			name = "autodrop on"
		}
		t.Run(name, func(t *testing.T) {
			pool, _ := miscCovClosedPool(t)
			logs := miscCovCaptureLogs(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			days := 45
			f := &bootFlags{auditRetentionDays: &days, auditRetentionAutodrop: &autodrop}

			startAuditRetention(ctx, f, store.NewPG(pool), nil)

			// The sweeper has made its first pass once the ensure (and, with autodrop, the drop) has failed.
			logs.await(t, "audit partition ensure failed")
			if autodrop {
				logs.await(t, "audit autodrop failed")
			}
			rec, ok := logs.find("audit retention policy NOT recorded")
			if !ok {
				t.Fatal("the failed policy write was not logged")
			}
			if v, _ := logs.attr(rec, "days"); v.Int64() != 45 {
				t.Errorf("days = %v, want the configured 45", v)
			}
			_, warned := logs.find("WARDYN_AUDIT_RETENTION_AUTODROP is on")
			if warned != autodrop {
				t.Errorf("autodrop warning logged = %v, want %v", warned, autodrop)
			}
			if _, dropped := logs.find("audit autodrop failed"); dropped != autodrop {
				t.Errorf("autodrop attempted = %v, want %v", dropped, autodrop)
			}
		})
	}
}

// --- audit split legacy --------------------------------------------------------------------------

func TestMiscCovAuditSplitLegacyNeedsADatabase(t *testing.T) {
	empty, blank := "", "   "
	timeout := time.Second
	f := &bootFlags{dsn: &blank, migrateDSN: &empty, migrateTimeout: &timeout}
	err := auditSplitLegacyMode(f)
	if err == nil || !strings.Contains(err.Error(), "needs a database") {
		t.Fatalf("err = %v, want a refusal naming the missing database", err)
	}
	if exitCodeOf(err) != 1 {
		t.Errorf("exit code = %d, want the generic 1 for a usage error", exitCodeOf(err))
	}
}

// An unreachable database fails the connect with the migrate-failed exit code, and the migrate DSN, when
// set, is the one dialled (the main DSN is never a fallback for it).
func TestMiscCovAuditSplitLegacyConnectFailureAndDSNChoice(t *testing.T) {
	timeout := time.Second
	mainDSN := "postgres://wardyn@127.0.0.1:1/maindb?connect_timeout=1&sslmode=disable"
	migDSN := "postgres://wardyn@127.0.0.1:1/migratedb?connect_timeout=1&sslmode=disable"
	empty := ""

	for _, tc := range []struct {
		name    string
		dsn     string
		migrate string
		wantDB  string
	}{
		{"main dsn alone", mainDSN, empty, "maindb"},
		{"migrate dsn wins", mainDSN, migDSN, "migratedb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, mig := tc.dsn, tc.migrate
			err := auditSplitLegacyMode(&bootFlags{dsn: &dsn, migrateDSN: &mig, migrateTimeout: &timeout})
			if err == nil {
				t.Fatal("no error against an unreachable database")
			}
			if exitCodeOf(err) != exitMigrateFailed {
				t.Errorf("exit code = %d, want %d", exitCodeOf(err), exitMigrateFailed)
			}
			if !strings.Contains(err.Error(), "connect db") {
				t.Errorf("err = %q, want it to say the connect failed", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantDB) {
				t.Errorf("err = %q, want it to name the database %q that was dialled", err.Error(), tc.wantDB)
			}
		})
	}
}
