// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// vanishedError is checkOnce's NotFound for a held pod with no witness: the object went without
// completion evidence (a stripped finalizer), so no later read can prove it stopped. It still wraps
// errTerminationUnknown; stillStopping does not count it as a pod that is still stopping.
type vanishedError struct{ err error }

func (e vanishedError) Error() string { return e.err.Error() }
func (e vanishedError) Unwrap() error { return e.err }

// stillStopping is runner-mode teardown's answer for a held run (teardownRunner). When the caller's ctx is
// live and the only thing missing is completion evidence that has not come yet (every leaf of err is
// errTerminationUnknown), and the run is not held as pending_delete, it wraps err in
// runner.ErrTeardownConfirming: the pods were deleted and are still stopping, and a retry of the kill
// completes the teardown. Any other answer (a read, delete or evidence-store error, a vanished pod, the
// pending_delete hold) is returned as it is. Either way nothing after the pods' wait has run: the
// NetworkPolicies stay until both pods are proven stopped and gone.
func stillStopping(ctx context.Context, err error) error {
	var pending *runner.ContainmentPendingError
	if ctx.Err() != nil || !onlyTerminating(err) || (errors.As(err, &pending) && pending.Kind != runner.ContainmentAgentOnly) {
		return err
	}
	return fmt.Errorf("%w: %w", runner.ErrTeardownConfirming, err)
}

// onlyTerminating reports whether every leaf of err's tree is errTerminationUnknown.
func onlyTerminating(err error) bool {
	switch e := err.(type) {
	case nil, vanishedError:
		return false
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		for _, x := range errs {
			if !onlyTerminating(x) {
				return false
			}
		}
		return len(errs) > 0
	case interface{ Unwrap() error }:
		return onlyTerminating(e.Unwrap())
	}
	return err == errTerminationUnknown
}
