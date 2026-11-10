// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.ProxyReviverHashed = (*Orchestrator)(nil)

// ReplaceProxyHashed forwards a hashed proxy replacement to ref's substrate
// (runner.ProxyReviverHashed). A substrate that cannot hash is refused with
// ErrReviveUnsupported before anything changes, never answered with no hash.
func (o *Orchestrator) ReplaceProxyHashed(ctx context.Context, ref string, cfgJSON []byte) (string, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return "", err
	}
	rv, ok := s.(runner.ProxyReviverHashed)
	if !ok {
		return "", runner.ErrReviveUnsupported
	}
	return rv.ReplaceProxyHashed(ctx, ref, cfgJSON)
}
