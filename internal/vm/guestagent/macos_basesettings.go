// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// The base-settings route applies the basesettings table in the guest. A tools update calls it so that a
// base prepared by an earlier build gets the same rows stage 1 writes into a new one.
//
//	POST /v1/tools/base-settings                  every row
//	POST /v1/tools/base-settings?scope=terminal   Terminal's kill and saved-state removal only (edit-base's finish)
//
// The route takes no body and no argv: a body, any other query, or an unknown scope is refused with 400 and
// nothing runs. The guest reads its own macOS version and build (sw_vers) and checks them against the
// fixed patterns before it renders the stamps. It adds no exec: ToolsExecAllowed is unchanged.
//
// Order: ask the processes to quit (SIGTERM, never SIGKILL, and only once the agent's desktop is up:
// desktopUp; before that they are skipped, so a login still coming up is never cut), write the rows, remove
// the saved state, then read the files back from disk with plutil. Root rows run
// directly. Agent rows run in the console session as the agent (asUserHome), never through AgentExec, which
// is outside the GUI session and would miss its preferences daemon. A row that failed or did not read back
// is skipped by name and does not fail the route. With nobody at the console, or another account there,
// every agent entry is skipped; the root rows still run.

// BaseSettingsScopeTerminal limits the route to Terminal's kill and saved-state removal.
const BaseSettingsScopeTerminal = "terminal"

// BaseSettingsResult is what the route did: how many entries applied, and the ones it skipped.
type BaseSettingsResult struct {
	Applied int                    `json:"applied"`
	Skipped []basesettings.Skipped `json:"skipped,omitempty"`
}

const (
	reasonNoConsole    = "nobody at the console"
	reasonOtherConsole = "the console is not the agent account's"
	reasonConsoleRead  = "the console user was not read"
	reasonDesktopNotUp = "the agent's desktop is not up yet (Dock and Finder)"
	// reasonNeedsFullDiskAccess is the skip reason of a protected row macOS refused to write.
	reasonNeedsFullDiskAccess = "needs Full Disk Access for the guest agent"

	// baseSettingsCmdTimeout bounds each command the route runs.
	baseSettingsCmdTimeout = 30 * time.Second
	// readBackTries and readBackGap give the preferences daemon time to write a file the route just changed.
	readBackTries = 3
)

var (
	readBackGap = 500 * time.Millisecond
	// baseSettingsMu serialises the route: two calls would interleave kills and writes.
	baseSettingsMu sync.Mutex
)

func (t *Tools) handleBaseSettings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	if len(q) > 1 || (len(q) == 1 && !q.Has("scope")) || (scope != "" && scope != BaseSettingsScopeTerminal) {
		http.Error(w, "base-settings takes no argv and only the scope terminal", http.StatusBadRequest)
		return
	}
	var one [1]byte
	if n, _ := io.ReadFull(io.LimitReader(r.Body, 1), one[:]); n > 0 || r.ContentLength > 0 {
		http.Error(w, "base-settings takes no body", http.StatusBadRequest)
		return
	}
	baseSettingsMu.Lock()
	defer baseSettingsMu.Unlock()
	b := &baseSettingsRun{t: t, ctx: r.Context(), run: t.run, terminalOnly: scope == BaseSettingsScopeTerminal}
	if b.run == nil {
		b.run = runCombined
	}
	if !b.terminalOnly {
		rel, err := b.release()
		if err != nil {
			slog.Warn("guest agent: tools mode: base settings: the guest's macOS version", "err", err)
			http.Error(w, "the guest's macOS version was not read: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if b.rows, err = basesettings.Rows(rel); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	b.uid, b.noSession = t.baseSettingsConsole(r.Context())
	b.apply()
	writeJSONAny(w, b.result)
}

// baseSettingsConsole is the console user's uid when it is the agent account, else why no agent session
// can be written to.
func (t *Tools) baseSettingsConsole(ctx context.Context) (uid uint32, why string) {
	if t.Desktop == nil || t.Desktop.Console == nil {
		return 0, reasonNoConsole
	}
	uid, err := t.Desktop.Console.Console(ctx)
	switch {
	case errors.Is(err, ErrNoConsoleUser):
		return 0, reasonNoConsole
	case err != nil:
		return 0, reasonConsoleRead
	case t.Desktop.AgentUID == 0 || uid != t.Desktop.AgentUID:
		return 0, reasonOtherConsole
	}
	return uid, ""
}

type baseSettingsRun struct {
	t            *Tools
	ctx          context.Context
	run          func(ctx context.Context, argv []string) ([]byte, int, error)
	terminalOnly bool
	rows         []basesettings.Row
	uid          uint32
	noSession    string // why no agent entry can run; "" when the agent has the console
	result       BaseSettingsResult
}

func (b *baseSettingsRun) skip(name, reason string) {
	b.result.Skipped = append(b.result.Skipped, basesettings.Skipped{Name: name, Reason: reason})
}

// exec runs argv and returns why it failed, "" when it exited 0 (or any of okCodes).
func (b *baseSettingsRun) exec(argv []string, okCodes ...int) (out string, why string) {
	ctx, cancel := context.WithTimeout(b.ctx, baseSettingsCmdTimeout)
	defer cancel()
	o, code, err := b.run(ctx, argv)
	switch {
	case err != nil:
		return "", "did not run"
	case code != 0 && !containsInt(okCodes, code):
		return string(o), "exit " + strconv.Itoa(code) + shortOutput(o)
	}
	return string(o), ""
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// shortOutput is ": <first line>" of a failed command's output, cut short. The commands are fixed and print
// no secret.
func shortOutput(o []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(o)), "\n")
	if line == "" {
		return ""
	}
	if len(line) > 120 {
		line = line[:120]
	}
	return ": " + line
}

// desktopUp is the run-start step's desktopUp, checked before each quit, its commands bounded as exec's.
func (b *baseSettingsRun) desktopUp() bool {
	ctx, cancel := context.WithTimeout(b.ctx, baseSettingsCmdTimeout)
	defer cancel()
	return b.t.Desktop.desktopUp(ctx, b.uid, b)
}

// Run makes the route's runner a CmdRunner for desktopUp: a non-zero exit is an error.
func (b *baseSettingsRun) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	o, code, err := b.run(ctx, append([]string{name}, args...))
	if err == nil && code != 0 {
		err = fmt.Errorf("%s: exit %d", name, code)
	}
	return o, err
}

// asAgent is the command to run argv in the agent's console session.
func (b *baseSettingsRun) asAgent(argv ...string) []string {
	cmd, args := asUserHome(b.uid, argv...)
	return append([]string{cmd}, args...)
}

// release reads sw_vers as root and checks both values against the fixed patterns.
func (b *baseSettingsRun) release() (basesettings.Release, error) {
	v, why := b.exec([]string{"/usr/bin/sw_vers", "-productVersion"})
	if why != "" {
		return basesettings.Release{}, fmt.Errorf("sw_vers -productVersion: %s", why)
	}
	bld, why := b.exec([]string{"/usr/bin/sw_vers", "-buildVersion"})
	if why != "" {
		return basesettings.Release{}, fmt.Errorf("sw_vers -buildVersion: %s", why)
	}
	return basesettings.ParseRelease(v, bld)
}

// apply runs kill, write, remove and read back, in that order.
func (b *baseSettingsRun) apply() {
	for _, k := range basesettings.Kills {
		if b.terminalOnly && !k.Terminal {
			continue
		}
		if b.noSession != "" {
			b.skip(k.Name, b.noSession)
			continue
		}
		if !b.desktopUp() {
			b.skip(k.Name, reasonDesktopNotUp)
			continue
		}
		// pkill exits 1 when nothing matched, which is fine: the process was not running.
		if _, why := b.exec([]string{"/usr/bin/pkill", "-TERM", "-U", strconv.FormatUint(uint64(b.uid), 10), "-x", k.Process}, 1); why != "" {
			b.skip(k.Name, why)
			continue
		}
		b.result.Applied++
	}
	written := map[string]bool{}
	for _, r := range b.rows {
		argv := r.Argv
		if r.User == basesettings.Agent {
			if b.noSession != "" {
				b.skip(r.Name, b.noSession)
				continue
			}
			argv = b.asAgent(argv...)
		}
		if o, why := b.exec(argv); why != "" {
			// The refusal can sit past the output's first line, which is all the reason keeps.
			if r.Protected && strings.Contains(o, "Could not write domain") {
				slog.Warn("guest agent: tools mode: base settings: a protected container refused the write", "row", r.Name, "output", strings.TrimSpace(o))
				why = reasonNeedsFullDiskAccess
			}
			b.skip(r.Name, why)
			continue
		}
		written[r.Name] = true
		b.result.Applied++
		if r.Read != nil {
			b.readOnce(r)
		}
	}
	for _, rm := range basesettings.Removals {
		if b.terminalOnly && !rm.Terminal {
			continue
		}
		if b.noSession != "" {
			b.skip(rm.Name, b.noSession)
			continue
		}
		if _, why := b.exec(b.asAgent("/bin/rm", "-rf", "--", rm.Path)); why != "" {
			b.skip(rm.Name, why)
			continue
		}
		b.result.Applied++
	}
	b.readBack(written)
}

// readOnce reads the row back by its command, as the row's user, once: a value other than the one written
// skips the row as not landed.
func (b *baseSettingsRun) readOnce(r basesettings.Row) {
	argv := r.Read.Argv
	if r.User == basesettings.Agent {
		argv = b.asAgent(argv...)
	}
	if out, why := b.exec(argv); why != "" || strings.TrimSpace(out) != r.Read.Want {
		b.skip(r.Name, "did not land")
		b.result.Applied--
	}
}

// readBack reads each file a written row has a check for, as root, and skips every row whose key does not hold
// what was written. A mismatch is read again a few times, since the preferences daemon writes a file a
// moment after the command returns.
func (b *baseSettingsRun) readBack(written map[string]bool) {
	var pending []basesettings.Row
	for _, r := range b.rows {
		if r.Check != nil && written[r.Name] {
			pending = append(pending, r)
		}
	}
	reasons := map[string]string{}
	for try := 0; try < readBackTries && len(pending) > 0; try++ {
		if try > 0 {
			select {
			case <-b.ctx.Done():
			case <-time.After(readBackGap):
			}
		}
		out := map[string]string{}
		var still []basesettings.Row
		for _, r := range pending {
			text, ok := out[r.Check.Path]
			if !ok {
				var why string
				if text, why = b.exec([]string{"/usr/bin/plutil", "-p", r.Check.Path}); why != "" {
					text = "\x00" + why
				}
				out[r.Check.Path] = text
			}
			switch {
			case strings.HasPrefix(text, "\x00"):
				reasons[r.Name] = "read back failed: " + strings.TrimPrefix(text, "\x00")
				still = append(still, r)
			case !r.Check.Holds(text):
				reasons[r.Name] = "not found when read back"
				still = append(still, r)
			default:
				delete(reasons, r.Name)
			}
		}
		pending = still
	}
	for _, r := range pending {
		b.skip(r.Name, reasons[r.Name])
		b.result.Applied--
	}
}
