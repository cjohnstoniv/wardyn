// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/broker"
)

// brokerTokenRevokes is the broker's optional GitHub token hand-back worker (broker.Broker.RunGitHubTokenRevokes).
type brokerTokenRevokes interface{ RunGitHubTokenRevokes(ctx context.Context) }

// brokerTokenSeals deletes a person's sealed GitHub tokens at erasure (broker.Broker.DeleteOwnerTokenSeals).
type brokerTokenSeals interface {
	DeleteOwnerTokenSeals(ctx context.Context, owner string) (int, error)
}

// Both hooks are type assertions on s.cfg.Broker, whose declared interface carries neither method, so a rename on one
// side would compile and silently drop the worker or the erasure step: pinned here.
var (
	_ brokerTokenRevokes = (*broker.Broker)(nil)
	_ brokerTokenSeals   = (*broker.Broker)(nil)
)

// startGitHubTokenRevokes starts the broker's GitHub token hand-back worker on every replica, when it has one.
func (s *Server) startGitHubTokenRevokes() {
	if w, ok := s.cfg.Broker.(brokerTokenRevokes); ok {
		s.goBackground(func() { w.RunGitHubTokenRevokes(s.watcherBaseCtx()) })
	}
}
