// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// capacityPollMax caps the poll backoff while a pod waits for room. A var only so tests can shorten it.
var capacityPollMax = 5 * time.Second

// startClock is one CreateSandbox's absolute start deadline, shared by the proxy pod's wait and the
// agent pod's wait so neither restarts it. Time a pod spends unplaceable for lack of room is tracked
// apart: it counts against capacityWait, not startTimeout, so a run that waited ten minutes for room
// still has its full start budget to pull an image. A change of stuck reason never resets either
// budget; only the pod's own scheduling verdict moves time between them.
type startClock struct {
	now          func() time.Time
	begin        time.Time
	startTimeout time.Duration
	capacityWait time.Duration

	blockedSince time.Time     // zero while no pod is waiting for room
	blocked      time.Duration // closed stretches of waiting for room
}

func (d *Driver) newStartClock() *startClock {
	return &startClock{now: time.Now, begin: time.Now(), startTimeout: d.cfg.StartTimeout, capacityWait: d.cfg.CapacityWait}
}

// capacityBlocked reports a pod the scheduler cannot place for lack of room.
func capacityBlocked(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && runner.CapacityBlockerReasons[c.Reason] {
			return true
		}
	}
	return false
}

// observe records the pod's latest scheduling verdict and reports whether it is waiting for room.
func (c *startClock) observe(pod *corev1.Pod) bool {
	waiting := c.capacityWait > 0 && capacityBlocked(pod)
	switch {
	case waiting && c.blockedSince.IsZero():
		c.blockedSince = c.now()
	case !waiting && !c.blockedSince.IsZero():
		c.blocked += c.now().Sub(c.blockedSince)
		c.blockedSince = time.Time{}
	}
	return waiting
}

func (c *startClock) capacityWaited() time.Duration {
	if c.blockedSince.IsZero() {
		return c.blocked
	}
	return c.blocked + c.now().Sub(c.blockedSince)
}

// expired is non-nil once either budget is spent; it wraps context.DeadlineExceeded so callers'
// timeout enrichment still applies.
func (c *startClock) expired() error {
	waited := c.capacityWaited()
	if waited > c.capacityWait {
		return fmt.Errorf("no machine had room for it within %s: %w", c.capacityWait, context.DeadlineExceeded)
	}
	if c.now().Sub(c.begin)-waited > c.startTimeout {
		return fmt.Errorf("did not start within %s: %w", c.startTimeout, context.DeadlineExceeded)
	}
	return nil
}

// remaining is how long one pod read may still run: what is left of the budget the pod is currently
// spending (the capacity budget while it waits for room, else the start budget), floored at one poll
// interval so a read's context is never created already expired.
func (c *startClock) remaining() time.Duration {
	left := c.startTimeout - (c.now().Sub(c.begin) - c.capacityWaited())
	if !c.blockedSince.IsZero() {
		left = min(left, c.capacityWait-c.capacityWaited())
	}
	return max(left, k8sPollInterval)
}

// poll runs check every k8sPollInterval (backing off to capacityPollMax while the pod waits for room)
// until it reports done, errs, the clock expires, or ctx ends. check returns whether the pod it just
// read is waiting for room. Each check runs under a context bounded by remaining, so an API server
// that never answers cannot hold a read past the budgets.
func (c *startClock) poll(ctx context.Context, check func(ctx context.Context) (done, waitingForRoom bool, err error)) error {
	interval := k8sPollInterval
	for {
		checkCtx, cancel := context.WithTimeout(ctx, c.remaining())
		done, waiting, err := check(checkCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				if expired := c.expired(); expired != nil {
					return expired
				}
				return fmt.Errorf("kubernetes did not answer a pod read within the start budget: %w", context.DeadlineExceeded)
			}
			return err
		}
		if done {
			return nil
		}
		if err := c.expired(); err != nil {
			return err
		}
		if waiting {
			interval = min(interval*2, capacityPollMax)
		} else {
			interval = k8sPollInterval
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// tolerateGetError reports a pod read failure the wait should ride out as "not yet": every error but
// NotFound (the pod was deleted under the wait, which no waiting fixes) and the read's own deadline
// (which poll turns into a budget verdict). A control-plane roll answers 503 for seconds, and the
// start and capacity budgets, not one blip, decide how long a run waits. The answer is kept in last
// so a timeout can name it; a good read clears it.
func tolerateGetError(err error, last *error) bool {
	if err == nil {
		*last = nil
		return false
	}
	if isNotFound(err) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	*last = err
	return true
}

// withLastGetError names the last tolerated apiserver answer on a timeout.
func withLastGetError(err, last error) error {
	if last == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w (last apiserver answer: %v)", err, last)
}
