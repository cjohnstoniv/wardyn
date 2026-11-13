// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// A run's guest signs the agent account in by auto-login at about the time the guest agent starts, so the
// per-host settings carry can find the agent already signed in and skip. With a second switch file beside
// the carry's (MacLoginHoldSwitchPath) the base rests with auto-login off: the guest boots to the login
// window, the agent carries with nobody at the console, writes auto-login back the way stage 1 does and
// restarts the guest, which then signs in. Once the agent is at the console auto-login is removed again, so
// a disk saved from that boot rests the same way. The agent does not serve while it holds the sign-in.
//
// Every path that finds auto-login off writes it back before the guest restarts, so a held boot signs in
// on the next one. Auto-login that cannot be written, a guest that does not restart, and a write that
// macOS keeps losing end as failed: the agent then serves, and Init refuses a run with a desktop that is
// still at the login window.
package guestagent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// loginWindowDomain is the preference domain stage 1 writes autoLoginUser to.
	loginWindowDomain = "/Library/Preferences/com.apple.loginwindow"
	// loginHoldBootFile, in MarkerDir, holds this boot's hold step and restart count. The boot clears it.
	loginHoldBootFile = "wardyn-login-hold"
	// loginHoldMaxRestarts is how many restarts in a row a hold may ask for without the agent signed in.
	loginHoldMaxRestarts = 3
)

var (
	// loginHoldRestartArgv restarts the guest as root through shutdown(8), which loginwindow does not
	// confirm. loginHoldRebootArgv is the second try.
	loginHoldRestartArgv = []string{"/sbin/shutdown", "-r", "now"}
	loginHoldRebootArgv  = []string{"/sbin/reboot"}
	// loginHoldPaths are the values of login_hold= and of a record's path.
	loginHoldPaths = []string{"off", "armed", "held", "restored", "fallback", "failed"}
	// loginHoldReason is a reason's shape.
	loginHoldReason = regexp.MustCompile(`^[a-z_]{0,32}$`)
	// loginHoldCarryLine is the shape of the carry line a record keeps.
	loginHoldCarryLine = regexp.MustCompile(`^host_prefs=[a-z]+( [a-z_]+=[a-z0-9_]+)*$`)
)

// LoginHold holds the agent's sign-in until the per-host settings carry has run. It is on only when the
// base has both MacLoginHoldSwitchPath and the carry's MacHostPrefsSwitchPath.
type LoginHold struct {
	Run     CmdRunner
	Console ConsoleUser
	// MarkerDir is the per-boot folder BootScrubDue reads.
	MarkerDir string
	// AgentUID is the account auto-login signs in.
	AgentUID uint32
	// Switch, CarrySwitch, Record and KCPassword are "" for MacLoginHoldSwitchPath,
	// MacHostPrefsSwitchPath, MacLoginHoldRecordPath and MacKCPasswordPath.
	Switch, CarrySwitch, Record, KCPassword string
	// Plist is the file auto-login is read back from; "" is the loginwindow domain's file.
	Plist string
	// LoginWait bounds Settle's wait for the agent at the console; zero is desktopLoginWait.
	LoginWait time.Duration
	// CarryBound bounds the carry inside a hold; zero is 45 s. The carry's child is bounded tighter.
	CarryBound time.Duration
	// GoDown is how long Start waits for the guest to go down after it asked for a restart; zero is 2 min.
	GoDown time.Duration
	// Every is the poll interval of read-backs and console checks; zero is 250 ms.
	Every time.Duration
	// ReadBack is how long a write may take to show in the file; zero is 5 s.
	ReadBack time.Duration
	// WatchEvery is how often Watch looks for the switch files; zero is 2 s.
	WatchEvery time.Duration
}

// LoginHoldStart is what Start decided for this boot. Settle reads it.
type LoginHoldStart struct {
	Path, Reason string
	// Restarts is how many restarts in a row the hold asked for before this boot went on.
	Restarts int
	// Rearm is set when the hold is on: Settle removes auto-login once the agent is signed in.
	Rearm bool
	// Restarting is set when the guest is going down for a restart Start asked for.
	Restarting bool
}

// loginHoldRecord is what a hold leaves on the disk for the next boot. Its file is root's.
type loginHoldRecord struct {
	Path     string `json:"path"`
	Reason   string `json:"reason,omitempty"`
	Restarts int    `json:"restarts"`
	Carry    string `json:"carry,omitempty"`
}

// Start runs when the guest agent starts, before the boot scrub. It returns the carry's host_prefs line
// and what this boot does. On a hold it does not return while the guest goes down for the restart it
// asked for; it returns with Restarting set once ctx ends, or as failed when the guest is still up after
// GoDown. Off a boot's first start (an Init happened) it only runs the carry.
func (h LoginHold) Start(ctx context.Context, carry func(context.Context) string) (string, LoginHoldStart) {
	if !BootScrubDue(h.MarkerDir) {
		return carry(ctx), LoginHoldStart{Path: "off"}
	}
	on := h.on()
	rec, hadRec := h.readRecord()
	if !on && !hadRec {
		// Never armed: auto-login stays as stage 1 left it, and nothing here reads or writes it.
		return carry(ctx), LoginHoldStart{Path: "off"}
	}
	auto := h.autoLogin(ctx)
	// after: the last boot ended in a restart this code asked for.
	after := rec.Path != "" && rec.Path != "armed"
	switch step, n := h.bootStep(); {
	case step != "":
		return h.again(ctx, step, n, auto, on, rec)
	case auto == MacAgentUser && after:
		return cmp.Or(rec.Carry, skippedCarry("hold_retry")),
			LoginHoldStart{Path: rec.Path, Reason: rec.Reason, Restarts: rec.Restarts, Rearm: on}
	case auto == MacAgentUser && on:
		return carry(ctx), LoginHoldStart{Path: "armed", Rearm: true}
	case auto != "":
		// The switch is off and auto-login is on again, or auto-login names another account.
		if auto == MacAgentUser {
			h.removeRecord()
		}
		return carry(ctx), LoginHoldStart{Path: "off"}
	}
	restarts := 0
	if after {
		restarts = rec.Restarts
	}
	return h.hold(ctx, carry, on, after, restarts)
}

// hold runs when auto-login is off and the hold is on or this code armed the base: it carries, writes
// auto-login back and restarts the guest, unless someone is at the console or a step fails.
func (h LoginHold) hold(ctx context.Context, carry func(context.Context) string, on, after bool, restarts int) (string, LoginHoldStart) {
	uid, cerr := h.Console.Console(ctx)
	if on && cerr == nil && uid == h.AgentUID {
		// The agent is signed in already: Settle removed auto-login earlier in this boot.
		return carry(ctx), LoginHoldStart{Path: "armed", Rearm: true}
	}
	st := LoginHoldStart{Path: "held", Restarts: restarts, Rearm: on}
	switch {
	case restarts >= loginHoldMaxRestarts:
		st.Path, st.Reason, st.Rearm = "failed", "restart_limit", false
	case !on:
		st.Path, st.Reason = "restored", "switch_off"
	case after:
		st.Path, st.Reason = "fallback", "autologin_lost"
	case cerr == nil:
		st.Path, st.Reason = "fallback", "console_taken"
	case !errors.Is(cerr, ErrNoConsoleUser):
		st.Path, st.Reason = "fallback", "console_unread"
	}
	h.setBootStep("hold", restarts+1)
	cctx, cancel := context.WithTimeout(ctx, cmp.Or(h.CarryBound, 45*time.Second))
	hostPrefs := carry(cctx)
	cancel()
	if !h.setAutoLogin(ctx) {
		return hostPrefs, LoginHoldStart{Path: "failed", Reason: "write_failed", Restarts: restarts}
	}
	if st.Path == "failed" {
		return hostPrefs, st
	}
	if _, err := h.Console.Console(ctx); err == nil {
		// Someone is at the console already: no restart under them.
		return hostPrefs, st
	}
	st.Restarts = restarts + 1
	return h.restart(ctx, hostPrefs, st)
}

// again runs when this service starts again in a boot where a hold began, after a crash or a restart of
// the service: it writes auto-login back if it is still off and asks for one more restart, never two.
func (h LoginHold) again(ctx context.Context, step string, n int, auto string, on bool, rec loginHoldRecord) (string, LoginHoldStart) {
	hostPrefs := cmp.Or(rec.Carry, skippedCarry("hold_retry"))
	st := LoginHoldStart{Path: cmp.Or(rec.Path, "held"), Reason: rec.Reason, Restarts: rec.Restarts, Rearm: on}
	if _, err := h.Console.Console(ctx); err == nil {
		return hostPrefs, st
	}
	if step == "retry" {
		st.Path, st.Reason, st.Rearm = "failed", "restart_failed", false
		return hostPrefs, st
	}
	reason := "restart_retry"
	if auto == "" {
		reason = "agent_restarted"
		if !h.setAutoLogin(ctx) {
			return hostPrefs, LoginHoldStart{Path: "failed", Reason: "write_failed", Restarts: n}
		}
	}
	h.setBootStep("retry", n)
	return h.restart(ctx, hostPrefs, LoginHoldStart{Path: "fallback", Reason: reason, Restarts: n, Rearm: on})
}

// restart records st for the next boot and restarts the guest.
func (h LoginHold) restart(ctx context.Context, hostPrefs string, st LoginHoldStart) (string, LoginHoldStart) {
	// A record that cannot be written still restarts: the next boot then reads as armed and signs in.
	_ = h.writeRecord(loginHoldRecord{Path: st.Path, Reason: st.Reason, Restarts: st.Restarts, Carry: hostPrefs})
	if h.runOK(ctx, loginHoldRestartArgv) || h.runOK(ctx, loginHoldRebootArgv) {
		select {
		case <-ctx.Done():
			st.Restarting = true
			return hostPrefs, st
		case <-time.After(cmp.Or(h.GoDown, 2*time.Minute)):
		}
	}
	st.Path, st.Reason, st.Rearm = "failed", "restart_failed", false
	return hostPrefs, st
}

// Settle waits, up to LoginWait, for the agent account at the console. When the hold is on it then
// removes auto-login, so a disk saved from this boot rests with it off; it leaves it on when the agent
// never signed in, the auto-login password file is gone, or the record cannot be written first. It returns the login_hold= status line: the
// path Start took and its reason, then whether the agent signed in.
func (h LoginHold) Settle(ctx context.Context, st LoginHoldStart) string {
	signedIn, rest := "unknown", "unknown"
	if st.Path != "off" {
		signedIn, rest = "no", "set"
		if h.awaitAgent(ctx) {
			signedIn = "yes"
			switch {
			case st.Rearm && regularFile(cmp.Or(h.KCPassword, MacKCPasswordPath)) && h.armedRecord() && h.clearAutoLogin(ctx):
				rest = "unset"
			case !st.Rearm && st.Path != "failed":
				h.removeRecord()
			}
		}
		if rest == "set" && h.autoLogin(ctx) == "" {
			rest = "unset"
		}
	}
	line := "login_hold=" + st.Path
	if st.Reason != "" {
		line += " reason=" + st.Reason
	}
	return fmt.Sprintf("%s restarts=%d signed_in=%s rest=%s at=%d", line, st.Restarts, signedIn, rest, time.Now().Unix())
}

// Watch runs on a boot that Start left off. When both switch files appear in this boot, made in edit-base
// or installed by a base's list, it arms the disk as Settle does: once the agent is at the console it
// removes auto-login, so the next boot holds. It returns Settle's line, or "" when ctx ends first or both
// files were already there, since Start then left the hold off for another reason.
func (h LoginHold) Watch(ctx context.Context) string {
	if h.on() {
		return ""
	}
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(cmp.Or(h.WatchEvery, 2*time.Second)):
		}
		if h.on() {
			return h.Settle(ctx, LoginHoldStart{Path: "armed", Rearm: true})
		}
	}
}

func (h LoginHold) on() bool {
	return regularFile(cmp.Or(h.Switch, MacLoginHoldSwitchPath)) && regularFile(cmp.Or(h.CarrySwitch, MacHostPrefsSwitchPath))
}

// awaitAgent says whether the agent account reached the console within LoginWait.
func (h LoginHold) awaitAgent(ctx context.Context) bool {
	end := time.Now().Add(cmp.Or(h.LoginWait, desktopLoginWait))
	for {
		if uid, err := h.Console.Console(ctx); err == nil && uid == h.AgentUID {
			return true
		}
		if !time.Now().Before(end) || !h.sleep(ctx) {
			return false
		}
	}
}

// autoLogin is autoLoginUser as the loginwindow domain's file holds it, the way loginwindow reads it at
// boot, not as a cache: "" when it is unset or cannot be read.
func (h LoginHold) autoLogin(ctx context.Context) string {
	out, err := h.Run.Run(ctx, "/usr/bin/plutil", "-extract", "autoLoginUser", "raw", "-o", "-", h.plist())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// setAutoLogin writes autoLoginUser as stage 1 does, twice, then once straight into the file, and says
// whether the file holds it within the read-back time.
func (h LoginHold) setAutoLogin(ctx context.Context) bool {
	writes := [][]string{
		{"/usr/bin/defaults", "write", loginWindowDomain, "autoLoginUser", MacAgentUser},
		{"/usr/bin/defaults", "write", loginWindowDomain, "autoLoginUser", MacAgentUser},
		{"/usr/bin/plutil", "-replace", "autoLoginUser", "-string", MacAgentUser, h.plist()},
	}
	for _, argv := range writes {
		if h.runOK(ctx, argv) && h.readsBack(ctx, MacAgentUser) {
			return true
		}
	}
	return false
}

// clearAutoLogin removes autoLoginUser and says whether the file no longer holds it.
func (h LoginHold) clearAutoLogin(ctx context.Context) bool {
	return h.runOK(ctx, []string{"/usr/bin/defaults", "delete", loginWindowDomain, "autoLoginUser"}) && h.readsBack(ctx, "")
}

// readsBack polls the file for want for up to ReadBack: the preferences daemon writes the file after
// defaults returns.
func (h LoginHold) readsBack(ctx context.Context, want string) bool {
	end := time.Now().Add(cmp.Or(h.ReadBack, 5*time.Second))
	for {
		if h.autoLogin(ctx) == want {
			return true
		}
		if !time.Now().Before(end) || !h.sleep(ctx) {
			return false
		}
	}
}

func (h LoginHold) sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(cmp.Or(h.Every, 250*time.Millisecond)):
		return true
	}
}

func (h LoginHold) runOK(ctx context.Context, argv []string) bool {
	_, err := h.Run.Run(ctx, argv[0], argv[1:]...)
	return err == nil
}

func (h LoginHold) plist() string { return cmp.Or(h.Plist, loginWindowDomain+".plist") }

func (h LoginHold) record() string { return cmp.Or(h.Record, MacLoginHoldRecordPath) }

// readRecord reads the record. The bool is whether any file is there: one that does not parse still says
// this code armed the base, so auto-login is written back.
func (h LoginHold) readRecord() (loginHoldRecord, bool) {
	fi, err := os.Lstat(h.record())
	if err != nil {
		return loginHoldRecord{}, false
	}
	var r loginHoldRecord
	b, err := os.ReadFile(h.record())
	if !fi.Mode().IsRegular() || fi.Size() > 4096 || err != nil || json.Unmarshal(b, &r) != nil ||
		!slices.Contains(loginHoldPaths, r.Path) || !loginHoldReason.MatchString(r.Reason) || r.Restarts < 0 || r.Restarts > 99 ||
		len(r.Carry) > 256 || (r.Carry != "" && !loginHoldCarryLine.MatchString(r.Carry)) {
		return loginHoldRecord{}, true
	}
	return r, true
}

func (h LoginHold) writeRecord(r loginHoldRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return replaceFile(h.record(), 0o644, func(f *os.File) error {
		_, err := f.Write(append(b, '\n'))
		return err
	})
}

func (h LoginHold) removeRecord() { _ = os.Remove(h.record()) }

// armedRecord writes the record of an armed base and says whether it is on the disk. Settle writes it
// before it removes auto-login: a base with auto-login off and no record is one Start leaves alone, so a
// guest that stops between the two must find the record there.
func (h LoginHold) armedRecord() bool {
	return h.writeRecord(loginHoldRecord{Path: "armed"}) == nil
}

// bootStep reads this boot's hold step ("hold" or "retry") and restart count; "" when no hold began.
func (h LoginHold) bootStep() (string, int) {
	b, err := os.ReadFile(filepath.Join(h.MarkerDir, loginHoldBootFile))
	if err != nil {
		return "", 0
	}
	var step string
	var n int
	if _, err := fmt.Sscanf(string(b), "%s %d", &step, &n); err != nil || step != "hold" && step != "retry" {
		return "hold", 0
	}
	return step, n
}

func (h LoginHold) setBootStep(step string, n int) {
	f, err := os.OpenFile(filepath.Join(h.MarkerDir, loginHoldBootFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %d\n", step, n)
	_ = f.Close()
}

// skippedCarry is the host_prefs line of a carry the hold did not run.
func skippedCarry(reason string) string {
	return fmt.Sprintf("host_prefs=skipped reason=%s at=%d", reason, time.Now().Unix())
}

func regularFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}
