// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"errors"
)

// ErrRevisionChanged is a guarded Put's answer when the row is no longer the
// one WithIfRevision named: someone else wrote, or deleted, it since. Nothing
// was written.
var ErrRevisionChanged = errors.New("secretstore: the row changed since it was read")

// ErrNoRevision is Revision's answer from a store that keeps no row revision.
var ErrNoRevision = errors.New("secretstore: this store keeps no row revision")

// Revisioned is the optional compare-and-set seam over a row: a holder that
// read a credential under a lock it may since have lost writes the replacement
// only if the row is still the one it read. Not part of Store, as MetaStore is
// not: a store without it (a test double) takes the unguarded Put, and a
// caller asserts it.
type Revisioned interface {
	// Revision names the current content of this view's OWN row of name,
	// opaquely, and is "" when the row does not exist. Equal revisions mean
	// no write landed between the two reads.
	Revision(ctx context.Context, name string) (string, error)
}

type ifRevisionKey struct{}

// WithIfRevision returns a context under which a Put writes only while this
// view's own row of the name still has revision rev (Revisioned.Revision), and
// otherwise returns ErrRevisionChanged without writing. rev "" means the row
// must not exist. A store that keeps no revision ignores it.
func WithIfRevision(ctx context.Context, rev string) context.Context {
	return context.WithValue(ctx, ifRevisionKey{}, rev)
}

// IfRevisionFrom is the revision WithIfRevision put on ctx, if any.
func IfRevisionFrom(ctx context.Context) (string, bool) {
	rev, ok := ctx.Value(ifRevisionKey{}).(string)
	return rev, ok
}

// RevisionOf reads st's revision of name when st keeps one: ok is false, with
// no error, for a store that does not.
func RevisionOf(ctx context.Context, st Store, name string) (rev string, ok bool, err error) {
	r, can := st.(Revisioned)
	if !can {
		return "", false, nil
	}
	rev, err = r.Revision(ctx, name)
	if errors.Is(err, ErrNoRevision) {
		return "", false, nil
	}
	return rev, err == nil, err
}
