// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A service grant's client secret record and its masks. The record is one JSON object in the operator namespace
// under serviceGrantSecretName; every read or write of it, and the mask call that follows, is made under that
// name's mutex, so the last value this replica applied is the newest one it has read.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// serviceGrantMaskGeneration is the masking registry's generation snapshot, taken before a credential is read.
func (s *Server) serviceGrantMaskGeneration(ctx context.Context) int64 {
	gen, err := s.cfg.MaskRegistry.GlobalGeneration(ctx)
	if err != nil {
		return 0
	}
	return gen
}

func (s *Server) noteServiceGrantMasked(name string) {
	s.svcGrant.mu.Lock()
	defer s.svcGrant.mu.Unlock()
	if s.svcGrant.masked == nil {
		s.svcGrant.masked = map[string]bool{}
	}
	s.svcGrant.masked[name] = true
}

// applyServiceGrantSecretMask makes value the one current mask of the record's name, retiring any other. The
// caller holds the name's mutex.
func (s *Server) applyServiceGrantSecretMask(gen int64, name, value string) error {
	if err := s.cfg.MaskRegistry.AddGlobal(gen, "", name, s.cfg.Now(), []byte(value)); err != nil {
		return err
	}
	s.noteServiceGrantMasked(name)
	return nil
}

// evictServiceGrantSecretMask retires every value masked under name. The caller holds the name's mutex.
func (s *Server) evictServiceGrantSecretMask(name string) {
	if err := s.cfg.MaskRegistry.EvictGlobal("", name, s.cfg.Now()); err != nil {
		slog.Warn("wardynd: could not retire a service grant secret's mask", slog.Any("err", err))
		return
	}
	s.svcGrant.mu.Lock()
	delete(s.svcGrant.masked, name)
	s.svcGrant.mu.Unlock()
}

// readServiceGrantRecord reads the record once and applies its client secret as the current mask. A record
// that is gone, not this shape or not maskable is a definitive failure with nothing sent.
func (s *Server) readServiceGrantRecord(ctx context.Context, name string) (serviceGrantRecord, *serviceGrantError) {
	st, ok := s.serviceGrantOperatorSecrets()
	if !ok {
		return serviceGrantRecord{}, svcFail(svcReasonUnavailable, false)
	}
	mu := s.svcGrant.nameLock(name)
	mu.Lock()
	defer mu.Unlock()
	gen := s.serviceGrantMaskGeneration(ctx)
	raw, err := st.Get(secretstore.WithPurpose(ctx, secretstore.PurposeServiceGrant), name)
	if errors.Is(err, secretstore.ErrNotFound) {
		s.evictServiceGrantSecretMask(name)
		return serviceGrantRecord{}, svcGone() // authoritative absence: no later failed read weakens it
	}
	if err != nil {
		return serviceGrantRecord{}, svcFail(svcReasonUnavailable, false)
	}
	rec, ok := parseServiceGrantRecord(raw)
	if !ok {
		return serviceGrantRecord{}, svcFail(svcReasonUnbound, true)
	}
	if !maskableServiceSecret(rec.ClientSecret) {
		return serviceGrantRecord{}, svcFail(svcReasonSecretUnmaskable, true)
	}
	if err := s.applyServiceGrantSecretMask(gen, name, rec.ClientSecret); err != nil {
		return serviceGrantRecord{}, svcFail(svcReasonUnavailable, false)
	}
	return rec, nil
}

// putServiceGrantRecord stores one whole record and applies its secret. After a failed Put it reads the record
// once, still under the mutex, and applies what is live: a failed write can leave either the old record or the
// new one in use. The caller holds the site-config document lock.
func (s *Server) putServiceGrantRecord(ctx context.Context, name string, rec serviceGrantRecord) error {
	st, ok := s.serviceGrantOperatorSecrets()
	if !ok {
		return errors.New("no secret store")
	}
	mu := s.svcGrant.nameLock(name)
	mu.Lock()
	defer mu.Unlock()
	gen := s.serviceGrantMaskGeneration(ctx)
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if perr := st.Put(ctx, name, raw); perr != nil {
		s.applyLiveServiceGrantRecord(ctx, st, name, gen)
		return perr
	}
	return s.applyServiceGrantSecretMask(gen, name, rec.ClientSecret)
}

// applyLiveServiceGrantRecord reads what the store holds and applies it, or retires every value when it holds none.
func (s *Server) applyLiveServiceGrantRecord(ctx context.Context, st secretstore.Store, name string, gen int64) {
	raw, err := st.Get(secretstore.WithPurpose(ctx, secretstore.PurposeServiceGrant), name)
	switch {
	case errors.Is(err, secretstore.ErrNotFound):
		s.evictServiceGrantSecretMask(name)
	case err == nil:
		if rec, ok := parseServiceGrantRecord(raw); ok && maskableServiceSecret(rec.ClientSecret) {
			_ = s.applyServiceGrantSecretMask(gen, name, rec.ClientSecret)
		}
	}
}

// deleteServiceGrantRecord deletes the record, with the revision when ifRev is set, and retires its masks.
// A delete that fails leaves the masks as they are: the value may still be stored.
func (s *Server) deleteServiceGrantRecord(ctx context.Context, name string, ifRev *string) error {
	st, ok := s.serviceGrantOperatorSecrets()
	if !ok {
		return errors.New("no secret store")
	}
	mu := s.svcGrant.nameLock(name)
	mu.Lock()
	defer mu.Unlock()
	dctx := ctx
	if ifRev != nil {
		dctx = secretstore.WithIfRevision(ctx, *ifRev)
	}
	if err := st.Delete(dctx, name); err != nil && !errors.Is(err, secretstore.ErrNotFound) {
		return err
	}
	s.evictServiceGrantSecretMask(name)
	if uid, ok := serviceGrantSecretUID(name); ok {
		s.dropServiceGrantCache(uid)
	}
	return nil
}

// reconcileServiceGrantMasks retires, on this replica, the mask of every secret name it has registered that no
// configured grant names any more or whose record is gone. It decides each name under that name's mutex and
// reads the configured grants again there, so a pass that read the settings before a route added a grant never
// retires the value the route has just applied.
func (s *Server) reconcileServiceGrantMasks(ctx context.Context) {
	s.svcGrant.mu.Lock()
	names := make([]string, 0, len(s.svcGrant.masked))
	for n := range s.svcGrant.masked {
		names = append(names, n)
	}
	s.svcGrant.mu.Unlock()
	sort.Strings(names)
	for _, name := range names {
		s.reconcileServiceGrantMask(ctx, name)
	}
}

func (s *Server) reconcileServiceGrantMask(ctx context.Context, name string) {
	mu := s.svcGrant.nameLock(name)
	if !mu.TryLock() {
		return // a read, a write or a delete of this record is in flight and applies its own mask; try the next pass
	}
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, serviceGrantStatusWriteBound)
	defer cancel()
	uid, _ := serviceGrantSecretUID(name)
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return
	}
	configured := false
	if sc.ModelProviders != nil {
		for _, p := range sc.ModelProviders.Providers {
			configured = configured || (p.UID == uid && p.OIDC != nil && p.OIDC.ServiceGrant != nil)
		}
	}
	gone := false
	if configured {
		rev, ok, rerr := s.serviceGrantRevision(ctx, name)
		if rerr != nil {
			return
		}
		gone = ok && rev == ""
	}
	if !configured || gone {
		s.evictServiceGrantSecretMask(name)
		s.dropServiceGrantCache(uid)
	}
}

// serviceGrantRowFor returns the provider holding a service grant under uid.
func serviceGrantRowFor(block *types.ModelProviders, uid string) (types.ModelProvider, bool) {
	if block != nil {
		for _, p := range block.Providers {
			if p.UID == uid && p.OIDC != nil && p.OIDC.ServiceGrant != nil {
				return p, true
			}
		}
	}
	return types.ModelProvider{}, false
}
