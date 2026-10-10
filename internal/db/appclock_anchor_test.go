// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0
package db

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

type clockRowFunc func(...any) error

func (f clockRowFunc) Scan(values ...any) error { return f(values...) }

type clockQueryFunc func(context.Context, string, ...any) pgx.Row

func (f clockQueryFunc) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	return f(ctx, q, args...)
}

func TestAppClockAnchorResponseDelayCannotAdvanceAdmission(t *testing.T) {
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	issued := base.Add(time.Minute)
	appAt := issued
	scanned := false
	q := clockQueryFunc(func(context.Context, string, ...any) pgx.Row {
		return clockRowFunc(func(values ...any) error {
			// The database answered at150ms; receipt occurs50ms later on the fast app clock.
			*values[0].(*time.Time) = base.Add(150 * time.Millisecond)
			appAt = issued.Add(200 * time.Millisecond)
			scanned = true
			return nil
		})
	})
	anchor, err := CaptureAppClock(context.Background(), q, func() time.Time {
		if !scanned {
			t.Fatal("app clock sampled before successful database receipt")
		}
		return appAt
	})
	if err != nil {
		t.Fatal(err)
	}
	if translated := anchor.Translate(issued); translated.After(base) {
		t.Fatalf("response delay advanced admission: %s > %s", translated, base)
	}
	// Waiting after capture must not change the instant. Clock rollback/future age
	// retains the old clamp: the translated timestamp never exceeds the DB anchor.
	if future := anchor.Translate(appAt.Add(time.Hour)); future != anchor.DatabaseAt {
		t.Fatalf("future age moved past anchor: %s", future)
	}
}

func TestAppClockCaptureFailureDoesNotSampleAppClock(t *testing.T) {
	failed := errors.New("database unavailable")
	q := clockQueryFunc(func(context.Context, string, ...any) pgx.Row {
		return clockRowFunc(func(...any) error { return failed })
	})
	_, err := CaptureAppClock(context.Background(), q, func() time.Time { t.Fatal("failed Scan sampled clock"); return time.Time{} })
	if !errors.Is(err, failed) {
		t.Fatalf("query failure lost: %v", err)
	}
}
