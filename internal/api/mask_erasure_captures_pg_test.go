// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

func TestPG_MaskErasureDelayedConsoleLogin(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	b := l.replicaMasking(newLabPool(t, l))
	lf := newLoginFixture(t, false)
	lf.srv = b.srv
	b.srv.cfg.Now = func() time.Time { return adoTestNow }
	b.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return lf.cfg, true, nil }
	lf.auth.AttachLoginGrantSink(b.srv)
	owner := lf.fake.Subject()
	l.personRun(owner, "login grant")
	barrier := maskHoldResponse(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- lf.login(t) }()
	maskWait(t, barrier.entered)
	maskEraseRequest(t, l, a, owner)
	close(barrier.release)
	if w := maskWait(t, done); !sessionIssued(w) {
		t.Fatalf("mask refusal prevented console login: %d", w.Code)
	}
	if _, found, err := b.srv.readADOEntraBlob(t.Context(), owner, lf.cfg.RowID); err != nil || found {
		t.Fatalf("stale login stored=%v, err=%v", found, err)
	}
	maskAssertErased(t, l, owner, a, b)
	if w := lf.login(t); !sessionIssued(w) {
		t.Fatal("new console sign-in failed")
	}
	if _, found, err := b.srv.readADOEntraBlob(t.Context(), owner, lf.cfg.RowID); err != nil || !found {
		t.Fatalf("new login stored=%v, err=%v", found, err)
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner); n != 1 {
		t.Fatalf("new login globals=%d", n)
	}
}

func TestPG_MaskErasureLoginSnapshotUnavailable(t *testing.T) {
	l := newMaskLab(t)
	maskPool := newLabPool(t, l)
	b := l.replicaMasking(maskPool)
	lf := newLoginFixture(t, false)
	lf.srv = b.srv
	b.srv.cfg.Now = func() time.Time { return adoTestNow }
	b.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return lf.cfg, true, nil }
	lf.auth.AttachLoginGrantSink(b.srv)
	maskPool.Close()
	if w := lf.login(t); !sessionIssued(w) {
		t.Fatalf("unavailable snapshot prevented console login: %d", w.Code)
	}
	if _, found, err := b.srv.readADOEntraBlob(t.Context(), lf.fake.Subject(), lf.cfg.RowID); err != nil || found {
		t.Fatalf("unfenced login captured=%v, %v", found, err)
	}
}

func TestPG_MaskErasureDelayedMintedPAT(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	b := l.replicaMasking(newLabPool(t, l))
	f := newMintFixture(t)
	f.srv = b.srv
	b.srv.cfg.Now = time.Now
	b.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }
	l.personRun(f.subject, "minted token")
	f.connect(t)
	barrier := maskHoldResponse(t)
	barrier.suffix = "/pats"
	done := make(chan error, 1)
	go func() {
		pat, err := b.srv.mintADOPAT(t.Context(), f.cfg, f.subject, "contoso", adoPATRequest{DisplayName: "mask erasure", Scope: "vso.code", ValidTo: time.Now().Add(time.Hour)})
		if pat.Token != "" {
			err = errors.New("returned an unmasked token")
		}
		done <- err
	}()
	maskWait(t, barrier.entered)
	maskEraseRequest(t, l, a, f.subject)
	close(barrier.release)
	if err := maskWait(t, done); !errors.Is(err, ErrADOEntraUnavailable) {
		t.Fatalf("delayed mint=%v", err)
	}
	if n := f.ado.Count(adofake.EndpointPatsCreate); n != 1 {
		t.Fatalf("actual token creates=%d", n)
	}
	maskAssertErased(t, l, f.subject, a, b)
}

func TestPG_MaskErasureDelayedFoundry(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "renewal"}[renewal], func(t *testing.T) {
			l := newMaskLab(t)
			fail := false
			a, b := erasureReplica(l, &fail), l.replicaMasking(newLabPool(t, l))
			f := newAzureFixture(t)
			f.srv.cfg.Secrets = b.sec
			f.srv.cfg.MaskRegistry = b.reg
			f.srv.locks.override = db.NewPGLocker(l.pool, 8)
			owner := f.fake.Subject()
			l.personRun(owner, "foundry erasure")
			var work func() error
			if renewal {
				if w := f.capture(t, owner, azUIDAnthropic); strings.Contains(w.Header().Get("Location"), "error") {
					t.Fatalf("initial foundry capture: %s", w.Header().Get("Location"))
				}
				cfg := f.app
				cfg.RowID = azUIDAnthropic
				work = func() error {
					_, err := f.srv.RedeemAzureFoundryAccess(t.Context(), cfg, owner, azUIDAnthropic, azureFoundryAudience)
					return err
				}
			} else {
				query, cookies, _ := f.begin(t, owner, azUIDAnthropic)
				work = func() error {
					w := f.callback(owner, query, cookies)
					if strings.HasSuffix(w.Header().Get("Location"), reasonStoreError) {
						return ErrADOEntraUnavailable
					}
					return errors.New("capture did not refuse erased generation")
				}
			}
			barrier := maskHoldResponse(t)
			done := make(chan error, 1)
			go func() { done <- work() }()
			maskWait(t, barrier.entered)
			maskEraseRequest(t, l, a, owner)
			close(barrier.release)
			if err := maskWait(t, done); !errors.Is(err, ErrADOEntraUnavailable) {
				t.Fatalf("late foundry response: %v", err)
			}
			maskAssertErased(t, l, owner, a, b)
			if w := f.capture(t, owner, azUIDAnthropic); strings.Contains(w.Header().Get("Location"), "error") {
				t.Fatalf("fresh foundry capture: %s", w.Header().Get("Location"))
			}
			if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner); n != 2 {
				t.Fatalf("fresh foundry rows=%d", n)
			}
		})
	}
}
