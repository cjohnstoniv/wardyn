// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// PrewrappedCheckUnavailable implements runner.PrewrappedImageChecker: the first local substrate that
// checks images answers; with none, this control plane has no Kubernetes substrate to check on.
func (o *Orchestrator) PrewrappedCheckUnavailable() string {
	if c := o.prewrappedChecker(); c != nil {
		return c.PrewrappedCheckUnavailable()
	}
	return "this control plane has no Kubernetes substrate"
}

// CheckPrewrappedImage implements runner.PrewrappedImageChecker on the same substrate. With none it
// returns no verdict, which runner.AggregateImageCheck refuses.
func (o *Orchestrator) CheckPrewrappedImage(ctx context.Context, attempt uuid.UUID, ref string) []runner.ImageArchVerdict {
	if c := o.prewrappedChecker(); c != nil {
		return c.CheckPrewrappedImage(ctx, attempt, ref)
	}
	return nil
}

func (o *Orchestrator) prewrappedChecker() runner.PrewrappedImageChecker {
	for _, s := range o.locals() {
		if c, ok := s.(runner.PrewrappedImageChecker); ok {
			return c
		}
	}
	return nil
}
