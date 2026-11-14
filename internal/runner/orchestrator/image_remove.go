// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.ImageRemover = (*Orchestrator)(nil)

// ImageRemove implements runner.ImageRemover by delegating to the first local substrate that implements
// it (the Docker one does), as ImagePresent does, so the daemon reclaims a superseded workspace image. With
// none (a Kubernetes-only deployment pulls fresh per launch, keeping nothing local) it removes nothing.
// A registered runner's images are its own and are never removed from here.
func (o *Orchestrator) ImageRemove(ctx context.Context, ref string) error {
	for _, s := range o.locals() {
		if ir, ok := s.(runner.ImageRemover); ok {
			return ir.ImageRemove(ctx, ref)
		}
	}
	return nil
}
