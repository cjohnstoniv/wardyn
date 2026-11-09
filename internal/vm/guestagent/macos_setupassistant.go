// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// MacSetupAssistantSwitchPath turns the run-start Setup Assistant step on: the step runs only on a base
// that has this file. Without it, the default, the step does nothing and the Apple Account pane can stay
// up in the window. /Library/Wardyn is root's, so only an administrator makes it.
const MacSetupAssistantSwitchPath = "/Library/Wardyn/setup-assistant-quit"

// setupAssistantWatch is how long endSetupAssistant looks for Setup Assistant once the desktop is up, when
// Desktop.SetupAssistantWatch is zero.
const setupAssistantWatch = 2 * time.Minute

// endSetupAssistant asks Setup Assistant to quit in the agent's desktop session, so a run's first window is
// the desktop. It does nothing unless the base has the switch file (MacSetupAssistantSwitchPath).
//
// launchd starts Setup Assistant's Apple Account pane as part of the agent's auto-login, and killing it
// while that login was still coming up ended the session: the guest fell back to the login window, whose
// password nobody knows, and auto-login runs once per boot. So the step acts only once the desktop is up
// (desktopUp), checks that again before every signal, and sends SIGTERM, never SIGKILL.
//
// It polls every SessionEnvEvery (2 s): for the desktop, up to SessionEnvWait (10 min), then for Setup
// Assistant, for the watch (2 min), so a relaunch is asked to quit too. pgrep and pkill exit 1 when nothing
// matched, which Run reports as an error.
func (d *Desktop) endSetupAssistant(ctx context.Context, agentUID uint32, run CmdRunner) {
	if !d.setupAssistantOn() {
		return
	}
	proc := ""
	for _, k := range basesettings.Kills {
		if k.Name == "kill.setup-assistant" {
			proc = k.Process
		}
	}
	if proc == "" {
		return
	}
	every := cmp.Or(d.SessionEnvEvery, sessionEnvPollEvery)
	end := time.Now().Add(cmp.Or(d.SessionEnvWait, sessionEnvPollWait))
	up := false
	for ctx.Err() == nil && time.Now().Before(end) {
		if d.desktopUp(ctx, agentUID, run) {
			if !up {
				up = true
				end = time.Now().Add(cmp.Or(d.SetupAssistantWatch, setupAssistantWatch))
			}
			if _, err := run.Run(ctx, "/usr/bin/pkill", "-TERM", "-U", strconv.FormatUint(uint64(agentUID), 10), "-x", proc); err == nil {
				slog.Info("guest agent: asked Setup Assistant to quit at run start")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// setupAssistantOn says whether the base turns endSetupAssistant on: its switch is a regular file.
func (d *Desktop) setupAssistantOn() bool {
	fi, err := os.Lstat(cmp.Or(d.SetupAssistantSwitch, MacSetupAssistantSwitchPath))
	return err == nil && fi.Mode().IsRegular()
}

// desktopUp says whether the agent's desktop session is up: the agent account at the console, with Dock
// and Finder running as the agent. The console alone is not enough: it is the agent's while the login is
// still starting the session.
func (d *Desktop) desktopUp(ctx context.Context, agentUID uint32, run CmdRunner) bool {
	if at, err := d.Console.Console(ctx); err != nil || at != agentUID {
		return false
	}
	uid := strconv.FormatUint(uint64(agentUID), 10)
	for _, app := range []string{"Dock", "Finder"} {
		if _, err := run.Run(ctx, "/usr/bin/pgrep", "-U", uid, "-x", app); err != nil {
			return false
		}
	}
	return true
}

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
			return fmt.Errorf("%w: %s %v after boot", ErrNeverAtDesktop, consoleSaw(uid, err), wait)
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
