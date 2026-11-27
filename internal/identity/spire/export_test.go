// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire

import (
	"context"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

// SpireBootWait and EntryX509SVIDTTL expose the constants to the external tests.
const (
	SpireBootWait    = spireBootWait
	EntryX509SVIDTTL = entryX509SVIDTTL
)

// TestSeams stand in for the constructor's reach outside the process. A nil Open is the real
// svidsource.Open, and a nil Now is time.Now.
type TestSeams struct {
	Env       map[string]string
	Namespace string
	Open      func(ctx context.Context, addr string, want spiffeid.ID) (svidsource.Source, error)
	Server    Server
	Now       func() time.Time
}

// ConstructForTest is the registered constructor over s.
func ConstructForTest(d identity.Deps, s TestSeams) (identity.Provider, error) {
	open, now := s.Open, s.Now
	if open == nil {
		open = svidsource.Open
	}
	if now == nil {
		now = time.Now
	}
	return construct(d, seams{
		getenv:        func(k string) string { return s.Env[k] },
		readNamespace: func() (string, error) { return s.Namespace, nil },
		open:          open,
		dial:          func(string, svidsource.Source, spiffeid.TrustDomain) (Server, error) { return s.Server, nil },
		now:           now,
	})
}
