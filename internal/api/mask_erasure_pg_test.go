// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type maskResponseBarrier struct {
	base             http.RoundTripper
	entered, release chan struct{}
	once             sync.Once
	suffix           string
}

func (b *maskResponseBarrier) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, b.suffix) {
		b.once.Do(func() {
			close(b.entered)
			select {
			case <-b.release:
			case <-r.Context().Done():
			}
		})
	}
	return resp, err
}

func maskHoldResponse(t *testing.T) *maskResponseBarrier {
	t.Helper()
	b := &maskResponseBarrier{base: http.DefaultTransport, suffix: "/token", entered: make(chan struct{}), release: make(chan struct{})}
	http.DefaultTransport = b
	t.Cleanup(func() {
		http.DefaultTransport = b.base
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	})
	return b
}

func maskWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("mask erasure work did not complete")
	}
	var zero T
	return zero
}

func maskEraseRequest(t *testing.T, l *maskLab, a replica, owner string) {
	t.Helper()
	if _, err := store.NewPG(l.pool).CreateAPIToken(t.Context(), types.APIToken{ID: uuid.New(), Principal: owner, Email: owner, Role: "user", UserType: types.UserTypeStandard, Name: "mask erasure fixture"}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	w := do(t, a.srv, http.MethodPost, "/api/v1/people/"+owner+"/erasure", adminToken, erasureBody("mask_copies"))
	if w.Code != http.StatusOK {
		t.Fatalf("mask_copies erase = %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner); n != 0 {
		t.Fatalf("erase left %d live rows", n)
	}
}

func maskAssertErased(t *testing.T, l *maskLab, owner string, replicas ...replica) {
	t.Helper()
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner); n != 0 {
		t.Fatalf("delayed credential restored %d live rows", n)
	}
	for _, rp := range replicas {
		if err := rp.st.Fresh(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if n := len(rp.reg.Snapshot(uuid.Nil)); n != 0 {
			t.Fatalf("replica retained %d erased globals", n)
		}
	}
}

func TestPG_MaskErasureDelayedAWSRefresh(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "existing"}[existing], func(t *testing.T) {
			l := newMaskLab(t)
			fail := false
			a := erasureReplica(l, &fail)
			b := l.replicaMasking(newLabPool(t, l))
			b.srv.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
			b.srv.locks.override = db.NewPGLocker(l.pool, 8)
			l.personRun(maskOwner, "mask erasure")
			putAWSSSOBlob(t, b.srv, awsSSOTestFixedNow.Add(-time.Minute))
			blob, found, err := b.srv.readAWSSSOBlob(t.Context(), awsSSOTestScope())
			if err != nil || !found {
				t.Fatalf("read seed: %v %v", found, err)
			}
			if existing {
				if err := b.srv.maskSSOBlob(blob, awsSSOTestScope()); err != nil {
					t.Fatal(err)
				}
			}
			calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
				_ = json.NewEncoder(w).Encode(awsSSOTokenResponse{AccessToken: "delayed-aws-new-access", RefreshToken: "delayed-aws-new-refresh", ExpiresIn: 3600})
			})
			barrier := maskHoldResponse(t)
			done := make(chan string, 1)
			go func() { _, reason := b.srv.refreshAWSSSOBlob(t.Context(), awsSSOTestScope(), blob); done <- reason }()
			maskWait(t, barrier.entered)
			maskEraseRequest(t, l, a, maskOwner)
			close(barrier.release)
			if reason := maskWait(t, done); reason != awsSSORefreshUnavailableSentence {
				t.Fatalf("late refresh reason=%q", reason)
			}
			if calls.Load() != 1 {
				t.Fatalf("token requests=%d", calls.Load())
			}
			current, found, err := b.srv.readAWSSSOBlob(t.Context(), awsSSOTestScope())
			if err != nil || !found || current.RefreshToken != blob.RefreshToken {
				t.Fatalf("late response changed credential: found=%v err=%v", found, err)
			}
			maskAssertErased(t, l, maskOwner, a, b)
			fresh := blob
			fresh.AccessToken = "deliberate-fresh-aws-access"
			fresh.RefreshToken = "deliberate-fresh-aws-refresh"
			storeSSOBlob(t, b.srv, fresh)
			current, found, err = b.srv.readAWSSSOBlob(t.Context(), awsSSOTestScope())
			if err != nil || !found {
				t.Fatalf("fresh capture read=%v, %v", found, err)
			}
			if err := b.srv.maskSSOBlob(current, awsSSOTestScope()); err != nil {
				t.Fatalf("new sign-in refused: %v", err)
			}
			if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, maskOwner); n != 3 {
				t.Fatalf("fresh sign-in live rows=%d", n)
			}
		})
	}
}

func TestPG_MaskErasureDelayedEntraRefresh(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "existing"}[existing], func(t *testing.T) {
			l := newMaskLab(t)
			fail := false
			a := erasureReplica(l, &fail)
			b := l.replicaMasking(newLabPool(t, l))
			f := newADOFixture(t)
			f.srv = b.srv
			b.srv.cfg.Now = func() time.Time { return adoTestNow }
			b.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }
			b.srv.locks.override = db.NewPGLocker(l.pool, 8)
			owner := f.fake.Subject()
			l.personRun(owner, "mask erasure")
			if w := f.capture(t, owner); w.Code != http.StatusFound || strings.Contains(w.Header().Get("Location"), "error") {
				t.Fatalf("initial capture = %d %s", w.Code, w.Header().Get("Location"))
			}
			before, found, err := b.srv.readADOEntraBlob(t.Context(), owner, f.cfg.RowID)
			if err != nil || !found {
				t.Fatalf("seed read=%v, %v", found, err)
			}
			if !existing {
				maskEraseRequest(t, l, a, owner)
			}
			barrier := maskHoldResponse(t)
			done := make(chan error, 1)
			go func() {
				_, err := b.srv.RedeemADOEntraAccess(t.Context(), f.cfg, owner, []string{f.cfg.Scopes[0]})
				done <- err
			}()
			maskWait(t, barrier.entered)
			maskEraseRequest(t, l, a, owner)
			close(barrier.release)
			if err := maskWait(t, done); !errors.Is(err, ErrADOEntraUnavailable) {
				t.Fatalf("late refresh=%v", err)
			}
			after, found, err := b.srv.readADOEntraBlob(t.Context(), owner, f.cfg.RowID)
			if err != nil || !found || after.RefreshToken != before.RefreshToken {
				t.Fatalf("late response changed credential: found=%v err=%v", found, err)
			}
			maskAssertErased(t, l, owner, a, b)
			if w := f.capture(t, owner); w.Code != http.StatusFound || strings.Contains(w.Header().Get("Location"), "error") {
				t.Fatalf("fresh sign-in = %d %s", w.Code, w.Header().Get("Location"))
			}
			if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner); n != 2 {
				t.Fatalf("fresh sign-in rows=%d", n)
			}
		})
	}
}

type maskReadBarrier struct {
	secretstore.Store
	entered, release chan struct{}
}

func (b *maskReadBarrier) For(owner string) secretstore.Store {
	return &maskReadBarrier{Store: b.Store.For(owner), entered: b.entered, release: b.release}
}
func (b *maskReadBarrier) Get(ctx context.Context, name string) ([]byte, error) {
	value, err := b.Store.Get(ctx, name)
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return value, err
}

func TestPG_MaskErasureAWSStaleReadCannotCapture(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), l.replicaMasking(newLabPool(t, l))
	l.personRun(maskOwner, "stale read")
	putAWSSSOBlob(t, b.srv, awsSSOTestFixedNow.Add(time.Hour))
	held := &maskReadBarrier{Store: b.srv.cfg.Secrets, entered: make(chan struct{}), release: make(chan struct{})}
	b.srv.cfg.Secrets = held
	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})
	done := make(chan error, 1)
	go func() {
		blob, found, err := b.srv.readAWSSSOBlob(t.Context(), awsSSOTestScope())
		if err == nil && !found {
			err = errors.New("seed missing")
		}
		if err == nil {
			err = b.srv.maskSSOBlob(blob, awsSSOTestScope())
		}
		done <- err
	}()
	maskWait(t, held.entered)
	maskEraseRequest(t, l, a, maskOwner)
	close(held.release)
	if err := maskWait(t, done); !errors.Is(err, secretmask.ErrErased) {
		t.Fatalf("stale capture=%v", err)
	}
	maskAssertErased(t, l, maskOwner, a, b)
}

func TestPG_MaskErasureDelayedEntraCapture(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	b := l.replicaMasking(newLabPool(t, l))
	f := newADOFixture(t)
	f.srv = b.srv
	b.srv.cfg.Now = func() time.Time { return adoTestNow }
	b.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }
	owner := f.fake.Subject()
	l.personRun(owner, "delayed first sign-in")
	authURL, cookies := f.signIn(t, owner, "")
	query := follow(t, authURL)
	barrier := maskHoldResponse(t)
	done := make(chan string, 1)
	go func() { w := f.callback(t, owner, query, cookies); done <- w.Header().Get("Location") }()
	maskWait(t, barrier.entered)
	maskEraseRequest(t, l, a, owner)
	close(barrier.release)
	if location := maskWait(t, done); !strings.HasSuffix(location, reasonStoreError) {
		t.Fatalf("stale sign-in location=%q", location)
	}
	if _, found, err := b.srv.readADOEntraBlob(t.Context(), owner, f.cfg.RowID); err != nil || found {
		t.Fatalf("stale capture stored=%v, err=%v", found, err)
	}
	maskAssertErased(t, l, owner, a, b)
	if w := f.capture(t, owner); strings.Contains(w.Header().Get("Location"), "error") {
		t.Fatalf("fresh sign-in refused=%s", w.Header().Get("Location"))
	}
}
