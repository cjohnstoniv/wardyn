// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

type heldPATCurrent struct {
	maskstore.Keys
	armed            atomic.Bool
	entered, release chan struct{}
}

func (k *heldPATCurrent) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	version, key, err := k.Keys.Current(ctx, owner, purpose)
	if err != nil || !k.armed.CompareAndSwap(true, false) {
		return version, key, err
	}
	close(k.entered)
	select {
	case <-k.release:
		return version, key, nil
	case <-ctx.Done():
		clear(key)
		return 0, nil, ctx.Err()
	}
}

func TestPG_MaskErasureMintedPATKeyDestroyedBeforeRegistration(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), l.replicaMasking(newLabPool(t, l))
	f := newMintFixture(t)
	f.srv = b.srv
	b.srv.cfg.Now = time.Now
	config := func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }
	a.srv.cfg.ADOEntra, b.srv.cfg.ADOEntra = config, config
	l.personRun(f.subject, "credential erasure")
	if _, err := store.NewPG(l.pool).CreateAPIToken(t.Context(), types.APIToken{ID: uuid.New(), Principal: f.subject, Email: f.subject, Role: "user", UserType: types.UserTypeStandard, Name: "credential erasure fixture"}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	held := &heldPATCurrent{Keys: b.sec.SubjectKeys(), entered: make(chan struct{}), release: make(chan struct{})}
	b.st = maskstore.New(newLabPool(t, l), held, b.reg)
	t.Cleanup(func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	})
	f.connect(t)
	response := maskHoldResponse(t)
	response.suffix = "/pats"
	type result struct {
		pat adoPAT
		err error
	}
	done := make(chan result, 1)
	go func() {
		pat, err := b.srv.mintADOPAT(t.Context(), f.cfg, f.subject, "contoso", adoPATRequest{DisplayName: "credential erasure", Scope: "vso.code", ValidTo: time.Now().Add(time.Hour)})
		done <- result{pat, err}
	}()
	maskWait(t, response.entered)
	before := b.reg.Snapshot(uuid.Nil)
	if len(before) == 0 {
		t.Fatal("fixture did not cache the sign-in values")
	}
	held.armed.Store(true)
	close(response.release)
	maskWait(t, held.entered)
	w := do(t, a.srv, http.MethodPost, "/api/v1/people/"+f.subject+"/erasure", adminToken, erasureBody("credentials"))
	if w.Code != http.StatusOK {
		t.Fatalf("credential erase returned %d: %s", w.Code, w.Body)
	}
	close(held.release)
	got := maskWait(t, done)
	if !errors.Is(got.err, ErrADOEntraUnavailable) || got.pat.Token != "" {
		t.Fatalf("mint error=%v returned token=%v", got.err, got.pat.Token != "")
	}
	if n := f.ado.Count(adofake.EndpointPatsCreate); n != 1 {
		t.Fatalf("actual token creates=%d", n)
	}
	if n := l.count(`SELECT count(*) FROM mask_values WHERE owner=$1 AND name LIKE 'ado-pat-%' AND NOT tombstone`, f.subject); n != 0 {
		t.Fatalf("destroyed-key PAT left %d live masks", n)
	}
	if n := l.count(`SELECT count(*) FROM mask_owner_erasures WHERE owner=$1`, f.subject); n != 0 {
		t.Fatal("credential erasure unexpectedly advanced the mask-copy fence")
	}
	for _, value := range before {
		if bytes.Contains(b.reg.Masker(uuid.Nil).Mask(value), value) {
			t.Fatal("credential erasure dropped a previously cached value before its grace")
		}
	}
}
