// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"errors"
	"testing"
)

// The console check's lock is three-valued. A lock key that reads false, or no lock key (macOS adds it only while
// the screen is locked), lets a window open on the console. A malformed key, or no dictionary, is unknown and
// refuses; a lock notification is a lock whatever the dictionary says.
func TestConsoleCheckLockState(t *testing.T) {
	onUnlocked := consoleReading{Dict: true, OnConsole: keyTrue, Locked: keyFalse}
	with := func(f func(*consoleReading)) consoleReading { r := onUnlocked; f(&r); return r }
	for name, tc := range map[string]struct {
		r    consoleReading
		lock lockState
		want error // nil, ErrNotAtMac, ErrLockUnknown or errConsoleUnreadable
	}{
		"on the console, known unlocked":   {onUnlocked, lockUnlocked, nil},
		"lock key true":                    {with(func(r *consoleReading) { r.Locked = keyTrue }), lockLocked, ErrNotAtMac},
		"lock key missing (unlocked Mac)":  {with(func(r *consoleReading) { r.Locked = keyAbsent }), lockUnlocked, nil},
		"lock key missing, lock notice":    {with(func(r *consoleReading) { r.Locked, r.LockNoticed = keyAbsent, true }), lockLocked, ErrNotAtMac},
		"lock key malformed":               {with(func(r *consoleReading) { r.Locked = keyMalformed }), lockUnknown, ErrLockUnknown},
		"no dictionary":                    {consoleReading{}, lockUnknown, ErrLockUnknown},
		"lock notice, dictionary unlocked": {with(func(r *consoleReading) { r.LockNoticed = true }), lockLocked, ErrNotAtMac},
		"lock notice, no dictionary":       {consoleReading{LockNoticed: true}, lockLocked, ErrNotAtMac},
		"not on the console":               {with(func(r *consoleReading) { r.OnConsole = keyFalse }), lockUnlocked, ErrNotAtMac},
		"not on the console, lock missing": {with(func(r *consoleReading) { r.OnConsole, r.Locked = keyFalse, keyAbsent }), lockUnlocked, ErrNotAtMac},
		"on-console key missing":           {with(func(r *consoleReading) { r.OnConsole = keyAbsent }), lockUnlocked, errConsoleUnreadable},
		"on-console key malformed":         {with(func(r *consoleReading) { r.OnConsole = keyMalformed }), lockUnlocked, errConsoleUnreadable},
	} {
		if got := tc.r.lock(); got != tc.lock {
			t.Errorf("%s: lock %v, want %v", name, got, tc.lock)
		}
		err := consoleCheck(tc.r)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: refused: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		// The three refusals stay apart: an unknown lock is never read as "not at the Mac", nor the reverse.
		if tc.want != ErrNotAtMac && errors.Is(err, ErrNotAtMac) || tc.want != ErrLockUnknown && errors.Is(err, ErrLockUnknown) {
			t.Errorf("%s: %v also matches another refusal", name, err)
		}
	}
}

// The cgo reader's numbers map onto the keys; any number it does not give is malformed, so it reads unknown.
func TestConsoleKeyOf(t *testing.T) {
	for n, want := range map[int]consoleKey{0: keyAbsent, 1: keyFalse, 2: keyTrue, 3: keyMalformed, 4: keyMalformed, -1: keyMalformed} {
		if got := consoleKeyOf(n); got != want {
			t.Errorf("consoleKeyOf(%d) = %v, want %v", n, got, want)
		}
	}
}
