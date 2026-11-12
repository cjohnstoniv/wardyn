// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proxyImageDelays are the waits between tries at a proxy image that cannot be pulled; the last repeats.
// A registry that does not have the image yet (a runner upgraded before its image is published) is
// usually a matter of minutes, so the first tries are quick and later ones cost the registry little.
var proxyImageDelays = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// proxyImageJitter is the share of a delay it may vary by either way, so runners restarted together do
// not pull together.
const proxyImageJitter = 0.1

// proxyImageDelay is the wait after the n-th failure (from 0).
func proxyImageDelay(n int) time.Duration {
	d := proxyImageDelays[min(n, len(proxyImageDelays)-1)]
	return d + time.Duration((rand.Float64()*2-1)*proxyImageJitter*float64(d))
}

// sleepCtx waits d, or returns ctx's error when it ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForProxyImage runs attempt until the proxy image is there instead of having the service exit and
// be restarted. Only a failure at the proxy-image stage is waited out; any other result, nil included,
// is returned at once, so a relay or engine refusal behaves as it did. Each failure goes to report
// with the time of the next try, and report(nil, zero) says the wait is over. When ctx ends (a stop) it
// returns the last failure joined with ctx's error, so the service still exits as a clean stop.
func waitForProxyImage(ctx context.Context, image string, attempt func(context.Context) error, report func(err error, next time.Time), sleep func(context.Context, time.Duration) error) error {
	var last, logged string
	for n := 0; ; n++ {
		err := attempt(ctx)
		var se *stageError
		if err == nil || !errors.As(err, &se) || se.stage != types.RunnerStageProxyImage {
			if err == nil && n > 0 {
				slog.InfoContext(ctx, "proxy image "+image+" is here")
				report(nil, time.Time{})
			}
			return err
		}
		last = err.Error()
		if ctx.Err() != nil {
			return fmt.Errorf("%w (stopped while waiting for the proxy image: %w)", err, ctx.Err())
		}
		d := proxyImageDelay(n)
		next := time.Now().Add(d)
		if last != logged {
			slog.WarnContext(ctx, "proxy image "+image+" is not available; waiting and trying again", "next_try", next.UTC().Format(time.RFC3339), "error", err)
			logged = last
		}
		report(err, next)
		if sleep(ctx, d) != nil {
			return fmt.Errorf("%w (stopped while waiting for the proxy image: %w)", err, ctx.Err())
		}
	}
}
