// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	saPath       = "/Users/agent/Library/Preferences/com.apple.SetupAssistant"
	termSaved    = "/Users/agent/Library/Saved Application State/com.apple.Terminal.savedState"
	diagPlist    = "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory.plist"
	baseSettings = "/v1/tools/base-settings"
)

// baseRig is a toolsRig whose run answers like a guest: it keeps what `defaults write` was given, so
// `plutil -p` can print it back, and it unwraps launchctl asuser so a test sees who ran what.
type baseRig struct {
	*toolsRig
	mu2      sync.Mutex
	log      []ranCmd
	store    map[string]map[string]string // plist path -> key -> printed value
	dropKeys map[string]bool              // keys whose write the guest silently loses
	failArgv func(argv []string) bool
	failOut  string // what a failed command prints; "boom" when empty
	swVers   map[string]string
}

// ranCmd is one command the route ran: asUID is the console uid it ran as (-1 for root, direct).
type ranCmd struct {
	argv  []string
	asUID int
}

func newBaseRig(t *testing.T) *baseRig {
	t.Helper()
	rig := &baseRig{toolsRig: newToolsRig(t), store: map[string]map[string]string{}, dropKeys: map[string]bool{},
		swVers: map[string]string{"-productVersion": "26.6.2\n", "-buildVersion": "25G83\n"}}
	rig.t.Desktop.AgentUID = 502
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.mu2.Lock()
		defer rig.mu2.Unlock()
		c := ranCmd{argv: argv, asUID: -1}
		// launchctl asuser <uid> /usr/bin/sudo -H -u #<uid> -- <argv...>
		if len(argv) > 8 && argv[0] == "/bin/launchctl" && argv[1] == "asuser" && argv[3] == "/usr/bin/sudo" && argv[4] == "-H" && argv[5] == "-u" && argv[6] == "#"+argv[2] && argv[7] == "--" {
			var uid int
			_, _ = fmt.Sscan(argv[2], &uid)
			c = ranCmd{argv: argv[8:], asUID: uid}
		}
		rig.log = append(rig.log, c)
		if rig.failArgv != nil && rig.failArgv(c.argv) {
			return []byte(cmp.Or(rig.failOut, "boom")), 1, nil
		}
		switch a := c.argv; {
		case a[0] == "/usr/bin/sw_vers" && len(a) == 2:
			return []byte(rig.swVers[a[1]]), 0, nil
		case a[0] == "/usr/bin/defaults" && slices.Contains(a, "write") && !slices.Contains(a, "-currentHost"):
			i := slices.Index(a, "write")
			path, key, typ, val := a[i+1], a[i+2], a[i+3], ""
			if path == "-g" {
				path, key, typ, val = "/Users/agent/Library/Preferences/.GlobalPreferences", a[i+2], a[i+3], a[i+4]
			} else {
				val = a[i+4]
			}
			if rig.dropKeys[key] {
				return nil, 0, nil
			}
			if rig.store[path+".plist"] == nil {
				rig.store[path+".plist"] = map[string]string{}
			}
			printed := val
			switch typ {
			case "-string":
				printed = `"` + val + `"`
			case "-bool":
				printed = map[string]string{"true": "1", "false": "0"}[val]
			}
			rig.store[path+".plist"][key] = printed
		case a[0] == "/usr/bin/defaults" && len(a) == 4 && a[1] == "read":
			if v, ok := rig.store[a[2]+".plist"][a[3]]; ok {
				return []byte(v + "\n"), 0, nil
			}
			return []byte("The domain/default pair of (x, y) does not exist\n"), 1, nil
		case a[0] == "/usr/bin/plutil" && len(a) == 3 && a[1] == "-p":
			var b strings.Builder
			b.WriteString("{\n")
			for k, v := range rig.store[a[2]] {
				fmt.Fprintf(&b, "  %q => %s\n", k, v)
			}
			b.WriteString("}\n")
			return []byte(b.String()), 0, nil
		}
		return nil, 0, nil
	}
	return rig
}

// post sends the route's request with body (nil: none) and decodes the answer.
func (r *baseRig) post(path, body string) (int, map[string]any) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if body == "" {
		req = httptest.NewRequest("POST", path, nil)
	}
	r.t.ToolsHandler().ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (r *baseRig) ran() []ranCmd {
	r.mu2.Lock()
	defer r.mu2.Unlock()
	return slices.Clone(r.log)
}

// index is the position of the first command run that has argv exactly as and the uid asUID, or -1.
func index(cmds []ranCmd, asUID int, argv ...string) int {
	return slices.IndexFunc(cmds, func(c ranCmd) bool { return c.asUID == asUID && slices.Equal(c.argv, argv) })
}

func skippedNames(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	got := map[string]string{}
	list, _ := out["skipped"].([]any)
	for _, e := range list {
		m := e.(map[string]any)
		got[m["name"].(string)] = m["reason"].(string)
	}
	return got
}

// The route's order is kill, write, remove, read back. Agent rows run in the console session as the agent
// (launchctl asuser with sudo -H -u), root rows run directly, and nothing runs as another uid.
func TestBaseSettingsRouteOrderAndUserSplit(t *testing.T) {
	rig := newBaseRig(t)
	code, out := rig.post(baseSettings, "")
	if code != 200 {
		t.Fatalf("base-settings = %d", code)
	}
	if sk := skippedNames(t, out); len(sk) != 0 {
		t.Fatalf("skipped %v", sk)
	}
	cmds := rig.ran()

	killSA := index(cmds, -1, "/usr/bin/pkill", "-TERM", "-U", "502", "-x", "Setup Assistant")
	killTerm := index(cmds, -1, "/usr/bin/pkill", "-TERM", "-U", "502", "-x", "Terminal")
	didSee := index(cmds, 502, "/usr/bin/defaults", "write", saPath, "DidSeeCloudSetup", "-bool", "true")
	stamp := index(cmds, 502, "/usr/bin/defaults", "write", saPath, "LastSeenCloudProductVersion", "-string", "26.6.2")
	global := index(cmds, 502, "/usr/bin/defaults", "write", "-g", "NSQuitAlwaysKeepsWindows", "-bool", "false")
	byHost := index(cmds, 502, "/usr/bin/defaults", "-currentHost", "write", "com.apple.loginwindow", "TALLogoutSavesState", "-bool", "false")
	mdutil := index(cmds, -1, "/usr/bin/mdutil", "-a", "-i", "off")
	autoSubmit := index(cmds, -1, "/usr/bin/defaults", "write", "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory", "AutoSubmit", "-bool", "false")
	chmod := index(cmds, -1, "/bin/chmod", "644", diagPlist)
	chgrp := index(cmds, -1, "/usr/bin/chgrp", "admin", diagPlist)
	remove := index(cmds, 502, "/bin/rm", "-rf", "--", termSaved)
	readSA := index(cmds, -1, "/usr/bin/plutil", "-p", saPath+".plist")
	for name, i := range map[string]int{"kill Setup Assistant": killSA, "kill Terminal": killTerm, "DidSee": didSee, "stamp": stamp, "global": global,
		"byHost": byHost, "mdutil": mdutil, "AutoSubmit": autoSubmit, "chmod": chmod, "chgrp": chgrp, "remove": remove, "read back": readSA} {
		if i < 0 {
			t.Fatalf("%s did not run as it should; ran:\n%v", name, cmds)
		}
	}
	lastKill, firstWrite, lastWrite := max(killSA, killTerm), min(didSee, stamp, global, byHost, mdutil, autoSubmit), max(didSee, stamp, global, byHost, mdutil, autoSubmit, chmod, chgrp)
	if !(lastKill < firstWrite && lastWrite < remove && remove < readSA) {
		t.Errorf("order: kills end %d, writes %d..%d, remove %d, read back %d", lastKill, firstWrite, lastWrite, remove, readSA)
	}
	if chmod < autoSubmit || chgrp < autoSubmit {
		t.Errorf("the diagnostics plist is chmod %d and chgrp %d before it is written at %d", chmod, chgrp, autoSubmit)
	}
	// The read-back covers each file the agent rows wrote and the diagnostics file.
	for _, p := range []string{saPath + ".plist", "/Users/agent/Library/Preferences/com.apple.HIToolbox.plist", "/Users/agent/Library/Preferences/.GlobalPreferences.plist", diagPlist} {
		if index(cmds, -1, "/usr/bin/plutil", "-p", p) < 0 {
			t.Errorf("%s was not read back", p)
		}
	}
	// Nothing ran as another user: every command is root's, direct, or the agent's uid.
	for _, c := range cmds {
		if c.asUID != -1 && c.asUID != 502 {
			t.Errorf("ran as uid %d: %q", c.asUID, c.argv)
		}
		// An agent preference never runs as root, and a root file never runs as the agent.
		joined := strings.Join(c.argv, " ")
		if c.asUID == -1 && strings.Contains(joined, "defaults") && strings.Contains(joined, "/Users/agent") {
			t.Errorf("an agent row ran as root: %q", c.argv)
		}
		if c.asUID == 502 && (strings.Contains(joined, "CrashReporter") || strings.Contains(joined, "mdutil")) {
			t.Errorf("a root row ran as the agent: %q", c.argv)
		}
	}
	if applied, _ := out["applied"].(float64); applied < 30 {
		t.Errorf("applied %v rows", out["applied"])
	}
}

// With root at the console (the login window) the route does not fail: every agent row, the kills and the
// removal are recorded as skipped, nothing runs in a session, and the root rows still apply.
func TestBaseSettingsRouteSkipsAgentRowsWithNobodyAtTheConsole(t *testing.T) {
	rig := newBaseRig(t)
	rig.t.Desktop.Console = fakeConsole{err: ErrNoConsoleUser}
	code, out := rig.post(baseSettings, "")
	if code != 200 {
		t.Fatalf("base-settings = %d", code)
	}
	sk := skippedNames(t, out)
	for _, n := range []string{"SetupAssistant.DidSeeCloudSetup", "SetupAssistant.LastSeenCloudProductVersion", "HIToolbox.AppleFnUsageType", "Global.NSQuitAlwaysKeepsWindows"} {
		if sk[n] != "nobody at the console" {
			t.Errorf("%s: skipped as %q", n, sk[n])
		}
	}
	if _, ok := sk["spotlight-off"]; ok {
		t.Error("a root row was skipped for the console")
	}
	cmds := rig.ran()
	for _, c := range cmds {
		if c.asUID != -1 || c.argv[0] == "/usr/bin/pkill" || c.argv[0] == "/bin/rm" {
			t.Errorf("ran with nobody at the console: %q as %d", c.argv, c.asUID)
		}
	}
	if index(cmds, -1, "/usr/bin/mdutil", "-a", "-i", "off") < 0 {
		t.Error("the root rows did not run")
	}
}

// Another account at the console is not the agent's: its session is never written to.
func TestBaseSettingsRouteNeverWritesAnotherUsersSession(t *testing.T) {
	rig := newBaseRig(t)
	rig.t.Desktop.Console = fakeConsole{uid: 501}
	if code, out := rig.post(baseSettings, ""); code != 200 || skippedNames(t, out)["SetupAssistant.DidSeeCloudSetup"] == "" {
		t.Fatalf("base-settings = %d %v", code, out)
	}
	for _, c := range rig.ran() {
		if c.asUID != -1 || strings.Contains(strings.Join(c.argv, " "), "501") {
			t.Errorf("ran for another account: %q as %d", c.argv, c.asUID)
		}
	}
}

// The route takes no body and no argv: a body, a query that names anything, or a scope it does not know
// is refused, and nothing runs.
func TestBaseSettingsRouteRefusesAnyInput(t *testing.T) {
	for name, tc := range map[string]struct{ path, body string }{
		"a JSON body":       {baseSettings, `{"argv":["/bin/sh","-c","id"]}`},
		"an empty object":   {baseSettings, `{}`},
		"a byte of body":    {baseSettings, `x`},
		"an argv query":     {baseSettings + "?argv=/bin/sh", ""},
		"a dest query":      {baseSettings + "?dest=/etc/passwd", ""},
		"an unknown scope":  {baseSettings + "?scope=all", ""},
		"a scope and extra": {baseSettings + "?scope=terminal&argv=x", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBaseRig(t)
			if code, _ := rig.post(tc.path, tc.body); code != 400 {
				t.Errorf("= %d, want 400", code)
			}
			if got := rig.ran(); len(got) != 0 {
				t.Errorf("ran %v", got)
			}
		})
	}
}

// Only the tools session serves it, and only as POST.
func TestBaseSettingsRouteIsPostOnly(t *testing.T) {
	rig := newBaseRig(t)
	w := httptest.NewRecorder()
	rig.t.ToolsHandler().ServeHTTP(w, httptest.NewRequest("GET", baseSettings, nil))
	if w.Code != 405 {
		t.Errorf("GET = %d, want 405", w.Code)
	}
}

// The edit-base finish asks for Terminal's kill and removal only.
func TestBaseSettingsRouteTerminalScope(t *testing.T) {
	rig := newBaseRig(t)
	if code, out := rig.post(baseSettings+"?scope=terminal", ""); code != 200 || len(skippedNames(t, out)) != 0 {
		t.Fatalf("= %d %v", code, out)
	}
	want := []ranCmd{
		{pgrepDock, -1},
		{pgrepFinder, -1},
		{[]string{"/usr/bin/pkill", "-TERM", "-U", "502", "-x", "Terminal"}, -1},
		{[]string{"/bin/rm", "-rf", "--", termSaved}, 502},
	}
	if got := rig.ran(); !slices.EqualFunc(got, want, func(a, b ranCmd) bool { return a.asUID == b.asUID && slices.Equal(a.argv, b.argv) }) {
		t.Fatalf("ran\n%v\nwant\n%v", got, want)
	}
}

var (
	pgrepDock   = []string{"/usr/bin/pgrep", "-U", "502", "-x", "Dock"}
	pgrepFinder = []string{"/usr/bin/pgrep", "-U", "502", "-x", "Finder"}
)

// The route never sends SIGKILL. It asks Setup Assistant and Terminal to quit with SIGTERM only once the
// agent's desktop is up, checked right before each signal: with Dock or Finder not running as the agent,
// both are skipped, nothing is signalled, and the rows still apply, so the update is never blocked.
func TestBaseSettingsRouteQuitsGentlyOnlyOnceTheDesktopIsUp(t *testing.T) {
	for _, down := range []string{"", "Dock", "Finder"} {
		t.Run("not running: "+cmp.Or(down, "none"), func(t *testing.T) {
			rig := newBaseRig(t)
			rig.failArgv = func(a []string) bool { return a[0] == "/usr/bin/pgrep" && a[len(a)-1] == down }
			code, out := rig.post(baseSettings, "")
			if code != 200 {
				t.Fatalf("base-settings = %d %v", code, out)
			}
			cmds := rig.ran()
			pkills := 0
			for i, c := range cmds {
				if slices.Contains(c.argv, "-KILL") || slices.Contains(c.argv, "-9") || slices.Contains(c.argv, "-SIGKILL") {
					t.Fatalf("a kill was sent: %q", c.argv)
				}
				if c.argv[0] != "/usr/bin/pkill" {
					continue
				}
				pkills++
				if c.argv[1] != "-TERM" || i < 2 || !slices.Equal(cmds[i-2].argv, pgrepDock) || !slices.Equal(cmds[i-1].argv, pgrepFinder) {
					t.Errorf("signal %q was not SIGTERM right after the Dock and Finder checks; ran:\n%v", c.argv, cmds)
				}
			}
			sk := skippedNames(t, out)
			switch {
			case down == "" && (pkills != 2 || len(sk) != 0):
				t.Errorf("desktop up: %d quits, skipped %v; want 2 and none", pkills, sk)
			case down != "" && (pkills != 0 || sk["kill.setup-assistant"] != reasonDesktopNotUp || sk["kill.terminal"] != reasonDesktopNotUp):
				t.Errorf("no %s: %d quits, skipped %v; want none sent and both skipped", down, pkills, sk)
			}
			if index(cmds, 502, "/usr/bin/defaults", "write", saPath, "DidSeeCloudSetup", "-bool", "true") < 0 {
				t.Error("the rows did not apply")
			}
		})
	}
}

// pkill exits 1 when no process matched; that is fine. Any other exit is a skipped row.
func TestBaseSettingsRouteKillNoProcessIsOK(t *testing.T) {
	rig := newBaseRig(t)
	rig.failArgv = func(a []string) bool { return false }
	base := rig.t.run
	rig.t.run = func(ctx context.Context, argv []string) ([]byte, int, error) {
		if argv[0] == "/usr/bin/pkill" {
			rig.mu2.Lock()
			rig.log = append(rig.log, ranCmd{argv: argv, asUID: -1})
			rig.mu2.Unlock()
			return nil, 1, nil
		}
		return base(ctx, argv)
	}
	if code, out := rig.post(baseSettings, ""); code != 200 || len(skippedNames(t, out)) != 0 {
		t.Fatalf("= %d %v", code, out)
	}
}

// A row that fails is skipped by name with the exit, and the rest still run. A key the guest loses is found
// by the read-back and skipped as well.
func TestBaseSettingsRouteSkipsFailedAndUnreadRows(t *testing.T) {
	rig := newBaseRig(t)
	rig.failArgv = func(a []string) bool { return slices.Equal(a, []string{"/usr/bin/mdutil", "-a", "-i", "off"}) }
	rig.dropKeys["DidSeeSiriSetup"] = true
	code, out := rig.post(baseSettings, "")
	if code != 200 {
		t.Fatalf("= %d", code)
	}
	sk := skippedNames(t, out)
	if !strings.Contains(sk["spotlight-off"], "exit 1") {
		t.Errorf("spotlight-off: %q", sk["spotlight-off"])
	}
	if !strings.Contains(sk["SetupAssistant.DidSeeSiriSetup"], "read back") {
		t.Errorf("DidSeeSiriSetup: %q", sk["SetupAssistant.DidSeeSiriSetup"])
	}
	if len(sk) != 2 {
		t.Errorf("skipped %v", sk)
	}
	if index(rig.ran(), 502, "/usr/bin/defaults", "write", saPath, "DidSeeTouchIDSetup", "-bool", "true") < 0 {
		t.Error("a failed row stopped the rest")
	}
}

// A sw_vers value outside the patterns is refused by the guest too: nothing is written and the host is told.
func TestBaseSettingsRouteRefusesAnUnpatternedSwVers(t *testing.T) {
	for name, vals := range map[string][2]string{"version": {"26.6.2; reboot\n", "25G83\n"}, "build": {"26.6.2\n", "25G83 x\n"}} {
		t.Run(name, func(t *testing.T) {
			rig := newBaseRig(t)
			rig.swVers["-productVersion"], rig.swVers["-buildVersion"] = vals[0], vals[1]
			if code, _ := rig.post(baseSettings, ""); code != 500 {
				t.Fatalf("= %d, want 500", code)
			}
			for _, c := range rig.ran() {
				if c.argv[0] != "/usr/bin/sw_vers" {
					t.Errorf("ran %q after a refused release", c.argv)
				}
			}
		})
	}
}

// The route adds no exec: tools mode still refuses every command it runs, so a host cannot ask for them.
func TestBaseSettingsAddsNoAllowedExec(t *testing.T) {
	for _, argv := range [][]string{
		{"/usr/bin/pkill", "-KILL", "-x", "Terminal"},
		{"/usr/bin/pkill", "-TERM", "-U", "502", "-x", "Terminal"},
		pgrepDock,
		pgrepFinder,
		{"/usr/bin/defaults", "write", saPath, "DidSeeCloudSetup", "-bool", "true"},
		{"/usr/bin/mdutil", "-a", "-i", "off"},
		{"/usr/bin/plutil", "-p", saPath + ".plist"},
		{"/bin/chmod", "644", diagPlist},
		{"/usr/bin/chgrp", "admin", diagPlist},
		{"/bin/rm", "-rf", "--", termSaved},
		{"/usr/bin/sw_vers", "-productVersion"},
	} {
		if err := ToolsExecAllowed(argv, MacToolsBinDir); err == nil && argv[0] != "/usr/bin/sw_vers" {
			t.Errorf("tools exec allows %q", argv)
		}
	}
}

const (
	safariPrefs = "/Users/agent/Library/Containers/com.apple.Safari/Data/Library/Preferences/com.apple.Safari"
	safariKey   = "WarnAboutFraudulentWebsites"
)

// Safari Safe Browsing is written as the agent in the session and read back once with `defaults read`,
// which must print 0.
func TestBaseSettingsRouteSafariSafeBrowsing(t *testing.T) {
	rig := newBaseRig(t)
	code, out := rig.post(baseSettings, "")
	if code != 200 || len(skippedNames(t, out)) != 0 {
		t.Fatalf("= %d %v", code, out)
	}
	cmds := rig.ran()
	w := index(cmds, 502, "/usr/bin/defaults", "write", safariPrefs, safariKey, "-bool", "false")
	r := index(cmds, 502, "/usr/bin/defaults", "read", safariPrefs, safariKey)
	if w < 0 || r < w {
		t.Fatalf("write at %d, read at %d; ran %v", w, r, cmds)
	}
	reads := 0
	for _, c := range cmds {
		if slices.Equal(c.argv, []string{"/usr/bin/defaults", "read", safariPrefs, safariKey}) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("read %d times, want once and no retry", reads)
	}
}

// A write the guest loses is recorded as skipped with "did not land", and the update is not failed.
func TestBaseSettingsRouteSafariReadBackMismatchIsSkipped(t *testing.T) {
	rig := newBaseRig(t)
	rig.dropKeys[safariKey] = true
	code, out := rig.post(baseSettings, "")
	if code != 200 {
		t.Fatalf("= %d", code)
	}
	sk := skippedNames(t, out)
	if len(sk) != 1 || sk["safari-safe-browsing"] != "did not land" {
		t.Fatalf("skipped %v", sk)
	}
}

// A Safari write macOS refuses with "Could not write domain" is skipped as needing Full Disk Access for the
// guest agent, not with the raw defaults error; a failing unprotected row keeps its exit reason.
func TestBaseSettingsRouteSafariRefusedWriteNeedsFullDiskAccess(t *testing.T) {
	rig := newBaseRig(t)
	rig.failOut = time.Now().Format("2006-01-02 15:04:05.000") + " defaults[1:2] \nCould not write domain /Users/agent/Library/Containers/com.apple.Safari/Data/Library/Preferences/com.apple.Safari; exiting"
	rig.failArgv = func(a []string) bool {
		return slices.Contains(a, "write") && slices.Contains(a, safariKey) || slices.Equal(a, []string{"/usr/bin/mdutil", "-a", "-i", "off"})
	}
	code, out := rig.post(baseSettings, "")
	if code != 200 {
		t.Fatalf("= %d", code)
	}
	sk := skippedNames(t, out)
	if got := sk["safari-safe-browsing"]; got != "needs Full Disk Access for the guest agent" {
		t.Fatalf("safari reason %q", got)
	}
	if got := sk["spotlight-off"]; !strings.HasPrefix(got, "exit 1") || strings.Contains(got, "Full Disk Access") {
		t.Fatalf("an unprotected row's reason %q, want the exit reason", got)
	}
}
