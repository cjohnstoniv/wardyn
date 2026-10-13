// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"errors"
)

// ErrGenerationChanged is a fenced Put's answer when the owner's credentials
// were erased after the write began (WithIfGeneration): nothing was written.
var ErrGenerationChanged = errors.New("secretstore: the owner's credentials were erased since this write began")

// ErrNoGeneration is Generation's answer from a store that keeps no erase
// generation.
var ErrNoGeneration = errors.New("secretstore: this store keeps no erase generation")

// Generational is the optional erase fence over one owner's namespace: a
// counter that EraseOwner advances before it deletes anything, and that a
// writer reads before it reads anything else it will write back. A fenced Put
// lands only while the counter still has the value the writer began with, so a
// write that began before an erase can never put a credential back behind it,
// whichever process it runs in. Not part of Store, as Revisioned and MetaStore
// are not: a store without it takes the unfenced Put, and EraseOwner says so.
//
// The counter is a number and nothing else: no credential, no hash of one, and
// no mark that refuses later writes. A write that begins after the erase
// reads the advanced value and lands.
type Generational interface {
	// Generation is this view's namespace's current erase generation. A
	// namespace never erased is 0.
	Generation(ctx context.Context) (int64, error)
	// AdvanceGeneration advances the generation and returns the new value. It
	// waits for every fenced Put already past its check, so a write that
	// passed the old value has landed before it returns, and EraseOwner's list
	// sees it.
	AdvanceGeneration(ctx context.Context) (int64, error)
}

type ifGenerationKey struct{ owner string }

// WithIfGeneration returns a context under which a Put into owner's namespace
// lands only while that namespace's erase generation is still gen, and
// otherwise returns ErrGenerationChanged without writing. The owner is part of
// the key so a context carried across namespaces fences only the one it was
// read for. A store that keeps no generation ignores it.
func WithIfGeneration(ctx context.Context, owner string, gen int64) context.Context {
	return context.WithValue(ctx, ifGenerationKey{owner}, gen)
}

// IfGenerationFrom is the generation WithIfGeneration put on ctx for owner.
func IfGenerationFrom(ctx context.Context, owner string) (gen int64, ok bool) {
	gen, ok = ctx.Value(ifGenerationKey{owner}).(int64)
	return gen, ok
}

// BeginWrite is the first step of every write of a person's stored credential:
// it reads view's erase generation and returns a context that fences the
// writes made under it. Call it before reading anything the write depends on,
// so an erase that lands after that read refuses the write. ok is false, with
// the context unchanged and no error, for a store that keeps no generation
// (the write is then protected by the caller's locks alone).
func BeginWrite(ctx context.Context, owner string, view Store) (_ context.Context, ok bool, err error) {
	g, can := view.(Generational)
	if !can {
		return ctx, false, nil
	}
	gen, err := g.Generation(ctx)
	if errors.Is(err, ErrNoGeneration) {
		return ctx, false, nil
	}
	if err != nil {
		return ctx, false, err
	}
	return WithIfGeneration(ctx, owner, gen), true, nil
}

// Superseded reports whether err is a guarded Put's refusal because the row
// changed (ErrRevisionChanged) or its owner was erased (ErrGenerationChanged)
// after the writer read. Neither is retried: the newer state is the answer.
func Superseded(err error) bool {
	return errors.Is(err, ErrRevisionChanged) || errors.Is(err, ErrGenerationChanged)
}
