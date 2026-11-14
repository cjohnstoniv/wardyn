// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"sync"
	"time"
)

// imageHolds is this process's image holds: the workspace-built image refs a run launch has selected
// and not yet finished dispatching. A superseded image is reclaimed (RemoveUnlessHeld) only when no
// launch holds it, so a refresh that publishes a new image cannot remove the old one between a launch
// choosing it and its sandbox being created from it. Holds are in memory, so they cover the launches
// of this process only. mu also guards every holdScope's refs.
var imageHolds = struct {
	mu   sync.Mutex
	refs map[string]int
}{refs: map[string]int{}}

// imageRemoveBound bounds one removal. It runs with the holds locked, so an engine that never answers would
// otherwise keep every launch of this process from taking or releasing a hold.
const imageRemoveBound = time.Minute

// holdScope is the holds one launch took, released together when it has dispatched.
type holdScope struct{ refs []string }

type holdScopeKey struct{}

// HoldImages opens a hold scope on ctx for one run launch: every image HoldImage then selects through
// the returned context stays held until release, which the launch calls once its dispatch has
// returned. A ctx that already carries a scope keeps it, and release is then a no-op.
func HoldImages(ctx context.Context) (context.Context, func()) {
	if _, ok := ctx.Value(holdScopeKey{}).(*holdScope); ok {
		return ctx, func() {}
	}
	s := &holdScope{}
	return context.WithValue(ctx, holdScopeKey{}, s), func() {
		imageHolds.mu.Lock()
		defer imageHolds.mu.Unlock()
		for _, ref := range s.refs {
			if imageHolds.refs[ref]--; imageHolds.refs[ref] <= 0 {
				delete(imageHolds.refs, ref)
			}
		}
		s.refs = nil
	}
}

// HoldImage holds ref for ctx's launch, before the launch checks the image is there and uses it. With
// no scope on ctx (a build that launches nothing) or no ref it does nothing. A hold taken while a
// removal of ref is under way waits for it, so the launch's own presence check then sees the image gone.
func HoldImage(ctx context.Context, ref string) {
	s, ok := ctx.Value(holdScopeKey{}).(*holdScope)
	if !ok || ref == "" {
		return
	}
	imageHolds.mu.Lock()
	defer imageHolds.mu.Unlock()
	imageHolds.refs[ref]++
	s.refs = append(s.refs, ref)
}

// RemoveUnlessHeld calls remove for ref unless a launch holds it, and reports a hold. The removal runs
// with the holds locked, so no launch can take a hold on ref and find it present while it is going,
// and for no longer than imageRemoveBound.
// ctx's own launch does not keep ref from itself: it reclaims an image it checked and then rebuilt.
func RemoveUnlessHeld(ctx context.Context, ref string, remove func(context.Context, string) error) (held bool, err error) {
	imageHolds.mu.Lock()
	defer imageHolds.mu.Unlock()
	own := 0
	if s, ok := ctx.Value(holdScopeKey{}).(*holdScope); ok {
		for _, r := range s.refs {
			if r == ref {
				own++
			}
		}
	}
	if imageHolds.refs[ref] > own {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, imageRemoveBound)
	defer cancel()
	return false, remove(ctx, ref)
}
