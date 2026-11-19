// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// desktopLoginWait is LoginWait's default. Auto-login takes well under a minute after boot.
const desktopLoginWait = 5 * time.Minute

// ErrNeverAtDesktop refuses the Init of a run with a desktop whose guest never signed the agent account in.
var ErrNeverAtDesktop = errors.New("the Mac guest never reached the agent's desktop")

// awaitAgentConsole waits until the agent account is at the console, up to LoginWait after booted. It
// returns nil once it is, ctx's error when ctx ends first, and ErrNeverAtDesktop, with what the console
// showed, when the wait is over. The time since booted is read once, so a clock change during the wait
// does not move its end.
func (d *Desktop) awaitAgentConsole(ctx context.Context, agentUID uint32, booted time.Time) error {
	wait := cmp.Or(d.LoginWait, desktopLoginWait)
	end := time.Now().Add(wait - min(max(time.Since(booted), 0), wait))
	every := cmp.Or(d.SessionEnvEvery, sessionEnvPollEvery)
	for {
		uid, err := d.Console.Console(ctx)
		if err == nil && uid == agentUID {
			return nil
		}
		left := time.Until(end)
		if left <= 0 {
			return fmt.Errorf("%w: %s %s after boot", ErrNeverAtDesktop, consoleSaw(uid, err), inWords(wait))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(every, left)):
		}
	}
}

// consoleSaw says in words who was at the console.
func consoleSaw(uid uint32, err error) string {
	switch {
	case errors.Is(err, ErrNoConsoleUser):
		return "still at the login window"
	case err != nil:
		return fmt.Sprintf("the console could not be read (%v)", err)
	}
	return fmt.Sprintf("another account (uid %d) at the console", uid)
}

// inWords says d as a person would: whole minutes as minutes, anything else as Go prints it.
func inWords(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "1 minute"
	case d > time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	}
	return d.String()
}
