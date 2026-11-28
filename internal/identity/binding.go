// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrBindingNotFound is GetBinding's typed refusal for a jti that has no binding row.
var ErrBindingNotFound = errors.New("identity: no token binding for that jti")

// TokenBinding is the delegation chain of one run token, keyed by the token's jti. A provider whose
// token cannot carry the chain (a SPIRE JWT-SVID holds only sub, aud and exp) writes it at mint and
// reads it back at Verify. It holds no workload: Claims.Workload comes from the signed ID alone.
type TokenBinding struct {
	JTI           string
	RunID         uuid.UUID
	SPIFFEID      string
	Sub           string
	Sponsor       string
	OperatorOwned bool
	Expiry        time.Time
}

// TokenBindingStore keeps TokenBindings. It has no delete by person: erasure is a store call from the
// API's erasure step, never through a provider.
type TokenBindingStore interface {
	// PutBinding writes b. A second put of the same jti succeeds when the row it finds is b in every field
	// (a deterministic signature can mint the same token twice) and is an error otherwise, never an
	// overwrite.
	PutBinding(ctx context.Context, b TokenBinding) error
	// GetBinding reads the binding of jti, or ErrBindingNotFound.
	GetBinding(ctx context.Context, jti string) (TokenBinding, error)
	// PruneBindings deletes at most 1000 rows, of any run, whose Expiry is before cutoff, and returns how
	// many it deleted.
	PruneBindings(ctx context.Context, cutoff time.Time) (int64, error)
}

// SameBinding is whether a and b agree in every field, the test PutBinding applies to a jti it already holds.
func SameBinding(a, b TokenBinding) bool {
	return a.JTI == b.JTI && a.RunID == b.RunID && a.SPIFFEID == b.SPIFFEID && a.Sub == b.Sub &&
		a.Sponsor == b.Sponsor && a.OperatorOwned == b.OperatorOwned && a.Expiry.Equal(b.Expiry)
}

// PruneBatch is the most rows one PruneBindings call deletes.
const PruneBatch = 1000
