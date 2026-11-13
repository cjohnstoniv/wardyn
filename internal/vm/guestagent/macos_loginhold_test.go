// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const holdAgentUID = 502

// holdLine is the login_hold= status line's shape.
var holdLine = regexp.MustCompile(`^login_hold=(off|armed|held|restored|fallback|failed)( reason=[a-z_]+)? restarts=[0-9]+ signed_in=(yes|no|unknown) rest=(unset|set|unknown) at=[0-9]+$`)

// agentDied is a panic that stands for the guest agent's process ending; launchd starts it again.
type agentDied struct{}

// simGuest is a macOS guest as the sign-in hold sees it. loginwindow reads autoLoginUser once per boot, as
// the boot starts, and signs the agent in a moment after the guest agent started when it names the agent
// and the auto-login password file is there. A restart clears the marker folder and boots again.
type simGuest struct {
	t       *testing.T
	root    string
	hold    LoginHold
	auto    string // autoLoginUser in the file; "" is unset
	console int    // the uid at the console; -1 is the login window
	signIn  bool   // this boot's loginwindow signs the agent in
	serving bool   // the agent went on past Start this boot
	boots   int
	asked   int // restarts the guest accepted
	cancel  context.CancelFunc
	calls   []string
	carries []int // who was at the console at each carry; -1 is nobody

	// Faults.
	defaultsFail   int    // so many more defaults writes fail
	defaultsNoop   int    // so many more defaults writes say done and leave the file as it was
	restartLate    bool   // the guest goes down a moment after the restart command returned
	plutilFail     bool   // plutil -replace fails
	lose           int    // so many more restarts lose the auto-login write
	restartFail    int    // so many more restart commands fail
	restartIgnored bool   // a restart command succeeds and the guest stays up
	crashInCarry   int    // the agent dies inside the carry so many more times
	crashAtRestart int    // the agent dies as it asks for the restart so many more times
	person         bool   // someone other than the agent signs in at the login window before the agent starts
	carryLine      string // what the carry answers; "" is two files carried
	carryBlocks    bool   // the carry runs until its bound
}

// newSim is an armed base: both switches, auto-login off, the record of a past re-arm and the password file.
func newSim(t *testing.T) *simGuest {
	t.Helper()
	root := stockLayout(t)
	g := &simGuest{t: t, root: root, console: -1}
	g.hold = LoginHold{Run: g, Console: g, MarkerDir: filepath.Join(root, "private/var/run"), AgentUID: holdAgentUID,
		Switch: root + MacLoginHoldSwitchPath, CarrySwitch: root + MacHostPrefsSwitchPath, Record: root + MacLoginHoldRecordPath,
		KCPassword: filepath.Join(root, "kcpassword"), Plist: filepath.Join(root, "com.apple.loginwindow.plist"),
		LoginWait: 200 * time.Millisecond, CarryBound: 200 * time.Millisecond, GoDown: 50 * time.Millisecond, Every: time.Millisecond,
		ReadBack: 20 * time.Millisecond}
	for _, p := range []string{g.hold.Switch, g.hold.CarrySwitch, g.hold.KCPassword} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.hold.writeRecord(loginHoldRecord{Path: "armed"}); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *simGuest) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	g.calls = append(g.calls, line)
	switch line {
	case "/usr/bin/plutil -extract autoLoginUser raw -o - " + g.hold.Plist:
		if g.auto == "" {
			return nil, errors.New("no value at that key path")
		}
		return []byte(g.auto + "\n"), nil
	case "/usr/bin/defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser agent":
		if g.defaultsFail > 0 {
			g.defaultsFail--
			return nil, errors.New("defaults failed")
		}
		if g.defaultsNoop > 0 {
			g.defaultsNoop--
			return nil, nil
		}
		g.auto = MacAgentUser
		return nil, nil
	case "/usr/bin/plutil -replace autoLoginUser -string agent " + g.hold.Plist:
		if g.plutilFail {
			return nil, errors.New("plutil failed")
		}
		g.auto = MacAgentUser
		return nil, nil
	case "/usr/bin/defaults delete /Library/Preferences/com.apple.loginwindow autoLoginUser":
		g.auto = ""
		return nil, nil
	case "/sbin/shutdown -r now", "/sbin/reboot":
		if g.auto != MacAgentUser {
			g.t.Errorf("a restart was asked with auto-login %q", g.auto)
		}
		if g.crashAtRestart > 0 {
			g.crashAtRestart--
			panic(agentDied{})
		}
		if g.restartFail > 0 {
			g.restartFail--
			return nil, errors.New("restart failed")
		}
		if g.restartLate {
			g.asked++
			time.AfterFunc(10*time.Millisecond, g.cancel)
			return nil, nil
		}
		if !g.restartIgnored {
			g.asked++
			g.cancel()
		}
		return nil, nil
	}
	g.t.Fatalf("unexpected command %q", line)
	return nil, nil
}

func (g *simGuest) Console(context.Context) (uint32, error) {
	if g.serving && g.signIn {
		g.console = holdAgentUID
	}
	if g.console < 0 {
		return 0, ErrNoConsoleUser
	}
	return uint32(g.console), nil
}

func (g *simGuest) carry(ctx context.Context) string {
	who := -1
	if uid, err := g.Console(ctx); err == nil {
		who = int(uid)
	}
	g.carries = append(g.carries, who)
	if g.crashInCarry > 0 {
		g.crashInCarry--
		panic(agentDied{})
	}
	if g.carryBlocks {
		select {
		case <-ctx.Done():
			return "host_prefs=failed reason=timeout console=none at=1"
		case <-time.After(5 * time.Second):
			g.t.Error("the carry ran unbounded")
		}
	}
	if g.carryLine != "" {
		return g.carryLine
	}
	return carriedLine + " console=none at=1"
}

func (g *simGuest) boot() {
	g.boots++
	if g.asked > 0 && g.lose > 0 {
		g.lose--
		g.auto = ""
	}
	if err := os.RemoveAll(g.hold.MarkerDir); err != nil {
		g.t.Fatal(err)
	}
	if err := os.Mkdir(g.hold.MarkerDir, 0o755); err != nil {
		g.t.Fatal(err)
	}
	g.console, g.serving = -1, false
	if g.person {
		g.console = 600
	}
	g.signIn = g.auto == MacAgentUser && regularFile(g.hold.KCPassword)
}

// start runs the agent's Start once and says whether the agent died in it.
func (g *simGuest) start() (hostPrefs string, st LoginHoldStart, died bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(agentDied); !ok {
				panic(r)
			}
			died = true
		}
	}()
	ctx, cancel := context.WithCancel(g.t.Context())
	defer cancel()
	g.cancel = cancel
	hostPrefs, st = g.hold.Start(ctx, g.carry)
	return hostPrefs, st, false
}

// serve starts the agent in this boot, again as launchd would after it died, until it goes on or the
// guest goes down for a restart. It returns the status lines when it went on.
func (g *simGuest) serve() (hostPrefs, hold string, restarting bool) {
	for starts := 0; starts < 5; starts++ {
		hp, st, died := g.start()
		if died {
			continue
		}
		if st.Restarting {
			return "", "", true
		}
		g.serving = true
		line := g.hold.Settle(g.t.Context(), st)
		if !holdLine.MatchString(line) {
			g.t.Errorf("status line %q is not the documented shape", line)
		}
		return hp, line, false
	}
	g.t.Fatal("the agent kept dying")
	return "", "", false
}

// run boots the guest until its agent goes on, at most 8 boots.
func (g *simGuest) run() (hostPrefs, hold string) {
	for g.boots < 8 {
		g.boot()
		if hp, line, restarting := g.serve(); !restarting {
			return hp, line
		}
	}
	g.t.Fatal("the guest restarted 8 times")
	return "", ""
}

func (g *simGuest) signedIn() bool { return g.console == holdAgentUID }

func (g *simGuest) ran(cmd string) int {
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, cmd) {
			n++
		}
	}
	return n
}

func wantHold(t *testing.T, line, prefix string) {
	t.Helper()
	if !strings.HasPrefix(line, prefix+" ") {
		t.Errorf("login_hold line = %q, want it to start %q", line, prefix)
	}
}

// Every failure the hold can see still writes auto-login back and ends with the agent signed in, and the
// disk rests with auto-login off again. Catches a path that leaves the guest at the login window.
func TestLoginHoldEveryPathEndsSignedIn(t *testing.T) {
	failedCarry := "host_prefs=failed reason=child_failed console=none at=1"
	for _, c := range []struct {
		name      string
		fault     func(g *simGuest)
		hold      string // the start of the login_hold line
		boots     int
		hostPrefs string // "" is the carried line
	}{
		{"clean", func(*simGuest) {}, "login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"the carry fails", func(g *simGuest) { g.carryLine = failedCarry }, "login_hold=held restarts=1 signed_in=yes rest=unset", 2, failedCarry},
		{"the carry runs past its bound", func(g *simGuest) { g.carryBlocks = true },
			"login_hold=held restarts=1 signed_in=yes rest=unset", 2, "host_prefs=failed reason=timeout console=none at=1"},
		{"defaults fails once", func(g *simGuest) { g.defaultsFail = 1 }, "login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"defaults fails, plutil writes", func(g *simGuest) { g.defaultsFail = 99 }, "login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"defaults says done and the file does not take it", func(g *simGuest) { g.defaultsNoop = 99 },
			"login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"the guest goes down a moment after the command", func(g *simGuest) { g.restartLate = true },
			"login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"the agent dies in the carry", func(g *simGuest) { g.crashInCarry = 1 },
			"login_hold=fallback reason=agent_restarted restarts=1 signed_in=yes rest=unset", 2, "host_prefs=skipped reason=hold_retry"},
		{"the agent dies as it restarts the guest", func(g *simGuest) { g.crashAtRestart = 1 },
			"login_hold=fallback reason=restart_retry restarts=1 signed_in=yes rest=unset", 2, ""},
		{"shutdown fails, reboot restarts", func(g *simGuest) { g.restartFail = 1 }, "login_hold=held restarts=1 signed_in=yes rest=unset", 2, ""},
		{"a restart loses the write", func(g *simGuest) { g.lose = 1 },
			"login_hold=fallback reason=autologin_lost restarts=2 signed_in=yes rest=unset", 3, ""},
		{"the hold switch is removed", func(g *simGuest) { _ = os.Remove(g.hold.Switch) },
			"login_hold=restored reason=switch_off restarts=1 signed_in=yes rest=set", 2, ""},
		{"the carry switch is removed", func(g *simGuest) { _ = os.Remove(g.hold.CarrySwitch) },
			"login_hold=restored reason=switch_off restarts=1 signed_in=yes rest=set", 2, ""},
		{"the record is damaged", func(g *simGuest) {
			_ = os.Remove(g.hold.Switch)
			_ = os.WriteFile(g.hold.Record, []byte("{"), 0o644)
		}, "login_hold=restored reason=switch_off restarts=1 signed_in=yes rest=set", 2, ""},
		{"the first boot after the switch", func(g *simGuest) { g.auto = MacAgentUser; g.hold.removeRecord() },
			"login_hold=armed restarts=0 signed_in=yes rest=unset", 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newSim(t)
			c.fault(g)
			hostPrefs, line := g.run()
			if !g.signedIn() {
				t.Fatalf("the guest is left at the login window after %d boots: %q", g.boots, line)
			}
			wantHold(t, line, c.hold)
			if g.boots != c.boots {
				t.Errorf("boots = %d, want %d", g.boots, c.boots)
			}
			if want := cmpStr(c.hostPrefs, carriedLine+" console=none at=1"); !strings.HasPrefix(hostPrefs, want) {
				t.Errorf("host_prefs line = %q, want the hold's carry %q", hostPrefs, want)
			}
			for i, who := range g.carries {
				if who != -1 && c.name != "the first boot after the switch" {
					t.Errorf("carry %d ran with uid %d at the console", i, who)
				}
			}
			rec, _ := g.hold.readRecord()
			switch {
			case strings.Contains(c.hold, "rest=unset"):
				if g.auto != "" || rec.Path != "armed" || rec.Restarts != 0 {
					t.Errorf("auto-login %q, record %+v; want off and armed for the next boot", g.auto, rec)
				}
			default:
				if g.auto != MacAgentUser || regularFile(g.hold.Record) {
					t.Errorf("auto-login %q, record there %v; want auto-login on and no record", g.auto, regularFile(g.hold.Record))
				}
			}
		})
	}
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Where no code in the guest can sign the agent in, the hold ends as failed and the agent serves, so a run
// with a desktop is refused at Init with the login window instead of waiting: auto-login that cannot be
// written, a guest that does not restart, and a write macOS keeps losing, which stops after three restarts.
func TestLoginHoldFailsWhereNoCodeCanSignIn(t *testing.T) {
	t.Run("auto-login cannot be written", func(t *testing.T) {
		g := newSim(t)
		g.defaultsFail, g.plutilFail = 99, true
		_, line := g.run()
		wantHold(t, line, "login_hold=failed reason=write_failed restarts=0 signed_in=no rest=unset")
		if g.boots != 1 || g.asked != 0 || g.ran("/sbin/") != 0 {
			t.Errorf("boots %d, restarts asked %d; want the agent serving at the first boot with no restart", g.boots, g.ran("/sbin/"))
		}
	})
	for name, fault := range map[string]func(*simGuest){
		"no restart command works": func(g *simGuest) { g.restartFail = 99 },
		"the guest stays up":       func(g *simGuest) { g.restartIgnored = true },
	} {
		t.Run(name, func(t *testing.T) {
			g := newSim(t)
			fault(g)
			_, line := g.run()
			wantHold(t, line, "login_hold=failed reason=restart_failed restarts=1 signed_in=no rest=set")
			if g.auto != MacAgentUser {
				t.Fatalf("auto-login %q after the failed restart, want it written back", g.auto)
			}
			// Whatever ends this boot, the next one signs in by itself and asks for no restart.
			g.restartFail, g.restartIgnored = 0, false
			calls := len(g.calls)
			_, line = g.run()
			if !g.signedIn() || g.asked != 0 {
				t.Fatalf("the next boot: signed in %v, restarts %d: %q", g.signedIn(), g.asked, line)
			}
			wantHold(t, line, "login_hold=held restarts=1 signed_in=yes rest=unset")
			for _, c := range g.calls[calls:] {
				if strings.HasPrefix(c, "/sbin/") {
					t.Errorf("the next boot ran %q", c)
				}
			}
		})
	}
	t.Run("the agent dies at every restart", func(t *testing.T) {
		g := newSim(t)
		g.crashAtRestart = 99
		g.boot()
		_, line, restarting := g.serve()
		if restarting || g.ran("/sbin/shutdown") != 2 || g.auto != MacAgentUser {
			t.Fatalf("restart commands %d, auto-login %q; want two tries in the boot, then the agent serving", g.ran("/sbin/shutdown"), g.auto)
		}
		wantHold(t, line, "login_hold=failed reason=restart_failed restarts=1 signed_in=no rest=set")
	})
	for name, fault := range map[string]func(*simGuest){
		"macOS keeps losing the write":                  func(g *simGuest) { g.lose = 99 },
		"macOS keeps losing the write, the switch gone": func(g *simGuest) { g.lose = 99; _ = os.Remove(g.hold.Switch) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newSim(t)
			fault(g)
			_, line := g.run()
			wantHold(t, line, "login_hold=failed reason=restart_limit restarts=3 signed_in=no rest=set")
			if g.boots != 4 || g.asked != 3 || g.auto != MacAgentUser {
				t.Errorf("boots %d, restarts %d, auto-login %q; want 3 restarts, then auto-login written and the agent serving", g.boots, g.asked, g.auto)
			}
		})
	}
	t.Run("auto-login cannot be written after the agent died", func(t *testing.T) {
		g := newSim(t)
		g.crashInCarry, g.defaultsFail, g.plutilFail = 1, 99, true
		_, line := g.run()
		wantHold(t, line, "login_hold=failed reason=write_failed")
		if g.boots != 1 || g.ran("/sbin/") != 0 {
			t.Errorf("boots %d, restart commands %d; want the agent serving at the first boot with no restart", g.boots, g.ran("/sbin/"))
		}
	})
}

// Someone signed in at the login window before the agent started: the carry skips, auto-login is written
// back and nothing restarts under them.
func TestLoginHoldNeverRestartsUnderSomeoneAtTheConsole(t *testing.T) {
	g := newSim(t)
	g.person = true
	_, line := g.run()
	wantHold(t, line, "login_hold=fallback reason=console_taken restarts=0 signed_in=no")
	if g.ran("/sbin/") != 0 || g.auto != MacAgentUser {
		t.Errorf("restart commands %d, auto-login %q; want none and auto-login written back", g.ran("/sbin/"), g.auto)
	}

	// The agent died in that boot and started again: still no restart under them.
	g = newSim(t)
	g.person, g.crashInCarry = true, 1
	g.run()
	if g.ran("/sbin/") != 0 {
		t.Errorf("restart commands %d after the agent started again with someone at the console; want none", g.ran("/sbin/"))
	}
}

// The agent started again in the boot that signed in, before any Init: it asks for no restart.
func TestLoginHoldRelaunchAfterSignInAsksNoRestart(t *testing.T) {
	g := newSim(t)
	g.run()
	calls := len(g.calls)
	_, line, restarting := g.serve()
	if restarting || g.ran("/sbin/shutdown") != 1 {
		t.Fatalf("a second start in the signed-in boot restarted the guest: %q", g.calls[calls:])
	}
	wantHold(t, line, "login_hold=armed restarts=0 signed_in=yes rest=unset")
}

// Off unless the base has both switch files, and a base it never armed is left alone: no read, write or
// restart. The carry's own switch never turns the hold on. Catches a hold without the opt-in.
func TestLoginHoldOffWithoutBothSwitches(t *testing.T) {
	for name, setup := range map[string]func(g *simGuest){
		"no switch, auto-login on": func(g *simGuest) {
			_ = os.Remove(g.hold.Switch)
			_ = os.Remove(g.hold.CarrySwitch)
		},
		"the carry's switch alone": func(g *simGuest) { _ = os.Remove(g.hold.Switch) },
		"the hold's switch alone":  func(g *simGuest) { _ = os.Remove(g.hold.CarrySwitch) },
		"a link at the switch": func(g *simGuest) {
			_ = os.Remove(g.hold.Switch)
			_ = os.Symlink(g.hold.CarrySwitch, g.hold.Switch)
		},
		"no switch, auto-login off": func(g *simGuest) {
			_ = os.Remove(g.hold.Switch)
			g.auto = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSim(t)
			g.hold.removeRecord()
			g.auto = MacAgentUser
			setup(g)
			auto := g.auto
			_, line := g.run()
			wantHold(t, line, "login_hold=off restarts=0 signed_in=unknown rest=unknown")
			if len(g.calls) != 0 || g.auto != auto {
				t.Errorf("commands %q, auto-login %q; want none and it unchanged", g.calls, g.auto)
			}
		})
	}
	if MacLoginHoldSwitchPath != "/Library/Wardyn/carry-before-login" || MacLoginHoldSwitchPath == MacHostPrefsSwitchPath {
		t.Fatalf("switch path = %q, which the docs do not name", MacLoginHoldSwitchPath)
	}
}

// After this boot's Init the agent's restart only runs the carry, which skips: nothing reads auto-login
// or restarts a live run.
func TestLoginHoldNothingAfterInit(t *testing.T) {
	g := newSim(t)
	g.boot()
	if err := os.WriteFile(filepath.Join(g.hold.MarkerDir, initMarkerFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, st, _ := g.start()
	if st.Path != "off" || len(g.calls) != 0 || len(g.carries) != 1 {
		t.Errorf("start after Init = %+v, commands %q, carries %d; want off, none, the carry once", st, g.calls, len(g.carries))
	}
}

// Auto-login stays on when the agent never reached the console, or when the auto-login password file is
// gone: removing it then would leave the next boot at the login window for good.
func TestLoginHoldSettleKeepsAutoLogin(t *testing.T) {
	g := newSim(t)
	g.auto = MacAgentUser
	g.boot()
	g.signIn = false
	g.serving = true
	wantHold(t, g.hold.Settle(t.Context(), LoginHoldStart{Path: "armed", Rearm: true}), "login_hold=armed restarts=0 signed_in=no rest=set")

	g.console = holdAgentUID
	_ = os.Remove(g.hold.KCPassword)
	wantHold(t, g.hold.Settle(t.Context(), LoginHoldStart{Path: "armed", Rearm: true}), "login_hold=armed restarts=0 signed_in=yes rest=set")
	if g.ran("/usr/bin/defaults delete") != 0 || g.auto != MacAgentUser {
		t.Errorf("auto-login %q after %d deletes; want it kept", g.auto, g.ran("/usr/bin/defaults delete"))
	}

	// A record that cannot be written: a base with auto-login off and no record is one Start leaves alone.
	_ = os.WriteFile(g.hold.KCPassword, []byte("x"), 0o600)
	g.hold.removeRecord()
	g.hold.Record = filepath.Join(g.root, "none", "login-hold.json")
	wantHold(t, g.hold.Settle(t.Context(), LoginHoldStart{Path: "armed", Rearm: true}), "login_hold=armed restarts=0 signed_in=yes rest=set")
	if g.ran("/usr/bin/defaults delete") != 0 || g.auto != MacAgentUser {
		t.Errorf("auto-login %q after %d deletes with no record written; want it kept", g.auto, g.ran("/usr/bin/defaults delete"))
	}
}

// A record of another shape is not trusted: it reads as an armed base, and its carry line is dropped.
func TestLoginHoldRecordShape(t *testing.T) {
	g := newSim(t)
	for _, body := range []string{`{"path":"held","restarts":-1}`, `{"path":"bogus"}`, `{"path":"held","carry":"host_prefs=carried; rm -rf /"}`, "not json"} {
		if err := os.WriteFile(g.hold.Record, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if r, there := g.hold.readRecord(); !there || r != (loginHoldRecord{}) {
			t.Errorf("record %s = %+v, %v; want an empty record that is there", body, r, there)
		}
	}
}

// Switch files made during a boot, in an edit-base session or by a base's list, arm the disk once the agent
// is at the console, so the next boot holds; a boot that had them already is not watched. Catches a base
// whose switches never take effect because only run clones ever saw them.
func TestLoginHoldWatchArmsWhenTheSwitchesAppear(t *testing.T) {
	g := newSim(t)
	g.hold.WatchEvery = time.Millisecond
	g.hold.removeRecord()
	_ = os.Remove(g.hold.Switch)
	g.auto = MacAgentUser
	if _, line := g.run(); !strings.HasPrefix(line, "login_hold=off ") {
		t.Fatalf("before the switch: %q", line)
	}
	done := make(chan string)
	go func() { done <- g.hold.Watch(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(g.hold.Switch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-done:
		wantHold(t, line, "login_hold=armed restarts=0 signed_in=yes rest=unset")
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not arm the disk 5 s after the switch appeared")
	}
	if rec, _ := g.hold.readRecord(); g.auto != "" || rec.Path != "armed" {
		t.Fatalf("auto-login %q, record %+v after the switch appeared; want off and armed", g.auto, rec)
	}
	if _, line := g.run(); !g.signedIn() || !strings.HasPrefix(line, "login_hold=held restarts=1 ") {
		t.Fatalf("the next boot = %q, signed in %v; want the hold", line, g.signedIn())
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if line := g.hold.Watch(ctx); line != "" {
		t.Errorf("Watch with both switches already there = %q, want nothing", line)
	}
}
