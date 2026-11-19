// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"testing"
	"time"
)

// A held image is not removed until every launch holding it has released it; a launch's own hold does
// not keep an image from that launch; a context with no scope holds nothing; and a hold taken while a
// removal is under way waits for it to finish.
func TestImageHoldsKeepALaunchingImage(t *testing.T) {
	var removed []string
	remove := func(_ context.Context, ref string) error { removed = append(removed, ref); return nil }
	ctx := context.Background()

	a, releaseA := HoldImages(ctx)
	b, releaseB := HoldImages(ctx)
	HoldImage(a, "img:old")
	HoldImage(b, "img:old")
	HoldImage(ctx, "img:unscoped")
	if inner, releaseInner := HoldImages(a); inner != a {
		t.Fatal("a context that carries a scope got a new one")
	} else {
		releaseInner() // a no-op: a's hold stays
	}
	if held, err := RemoveUnlessHeld(ctx, "img:old", remove); !held || err != nil || len(removed) != 0 {
		t.Fatalf("held=%v err=%v removed=%v; want a held image kept", held, err, removed)
	}
	if held, _ := RemoveUnlessHeld(a, "img:old", remove); !held || len(removed) != 0 {
		t.Fatalf("held=%v removed=%v; want it kept for the other launch that holds it", held, removed)
	}
	releaseA()
	if held, _ := RemoveUnlessHeld(ctx, "img:old", remove); !held || len(removed) != 0 {
		t.Fatalf("held=%v removed=%v; want it kept while the second launch holds it", held, removed)
	}
	HoldImage(b, "img:rebuilt")
	if held, err := RemoveUnlessHeld(b, "img:rebuilt", remove); held || err != nil || len(removed) != 1 {
		t.Fatalf("held=%v err=%v removed=%v; want a launch to reclaim an image only it holds", held, err, removed)
	}
	releaseB()
	releaseB() // a second release changes nothing
	if held, err := RemoveUnlessHeld(ctx, "img:old", remove); held || err != nil || len(removed) != 2 {
		t.Fatalf("held=%v err=%v removed=%v; want it removed once no launch holds it", held, err, removed)
	}
	if held, _ := RemoveUnlessHeld(ctx, "img:unscoped", remove); held || len(removed) != 3 {
		t.Fatalf("held=%v removed=%v; a context with no scope must hold nothing", held, removed)
	}

	started, finish, holding := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = RemoveUnlessHeld(ctx, "img:going", func(context.Context, string) error {
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	c, releaseC := HoldImages(ctx)
	defer releaseC()
	go func() { HoldImage(c, "img:going"); close(holding) }()
	select {
	case <-holding:
		t.Fatal("a hold was taken while the image was being removed")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	<-holding
}

// A removal runs with the holds locked, so it is bounded even when its caller's context is not: an
// engine that never answers cannot keep every launch from taking or releasing a hold.
func TestImageRemovalIsBounded(t *testing.T) {
	var left time.Duration
	bounded := false
	held, err := RemoveUnlessHeld(context.Background(), "img:bounded", func(ctx context.Context, _ string) error {
		var deadline time.Time
		deadline, bounded = ctx.Deadline()
		left = time.Until(deadline)
		return nil
	})
	if held || err != nil || !bounded || left <= 0 || left > imageRemoveBound {
		t.Fatalf("held=%v err=%v bounded=%v with %s left; want the removal run under a deadline of at most %s", held, err, bounded, left, imageRemoveBound)
	}
}
