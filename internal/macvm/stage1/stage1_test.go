// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package stage1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/stage1/stage1test"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vmnet"
	"golang.org/x/crypto/ssh"
)

const (
	admin    = "wardyn-admin"
	provPass = "provisioned-Pass_0123456789abcdefghijklmnopqrstuv"
	code     = "Kp7wQm3Z"
	newPass  = "replacement-Pass_0123456789abcdefghijklmnopqrstu"
)

var sudoWords = []string{"/usr/bin/sudo", "-k", "-S", "-p", "", "--"}

func root(argv ...string) []string { return append(append([]string(nil), sudoWords...), argv...) }

// wantArgv is stage 1's whole sequence on the D1(a) path, word for word.
var wantArgv = [][]string{
	root("/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", "755", "/usr/local/libexec"),
	root("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "755", "/dev/null", "/usr/local/libexec/wardyn-guest-agent"),
	root("/bin/dd", "of=/usr/local/libexec/wardyn-guest-agent"),
	root("/usr/bin/shasum", "-a", "256", "/usr/local/libexec/wardyn-guest-agent"),
	root("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "644", "/dev/null", "/Library/LaunchDaemons/com.wardyn.guest-agent.plist"),
	root("/bin/dd", "of=/Library/LaunchDaemons/com.wardyn.guest-agent.plist"),
	root("/usr/sbin/sysadminctl", "-addUser", "agent", "-fullName", "agent", "-password", "-"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.loginwindow", "autoLoginUser", "agent"),
	root("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "600", "/dev/null", "/etc/kcpassword"),
	root("/bin/dd", "of=/etc/kcpassword"),
	root("/bin/launchctl", "enable", "system/com.wardyn.guest-agent"),
	root("/bin/launchctl", "bootstrap", "system", "/Library/LaunchDaemons/com.wardyn.guest-agent.plist"),
	root("/bin/launchctl", "enable", "system/com.apple.screensharing"),
	root("/bin/launchctl", "bootstrap", "system", "/System/Library/LaunchDaemons/com.apple.screensharing.plist"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.RemoteManagement", "VNCLegacyConnectionsEnabled", "-bool", "true"),
	root("/usr/bin/pmset", "-a", "sleep", "0", "displaysleep", "0", "disksleep", "0"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.screensaver", "askForPassword", "-int", "0"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.screensaver", "idleTime", "-int", "0"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticCheckEnabled", "-bool", "false"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticDownload", "-bool", "false"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticallyInstallMacOSUpdates", "-bool", "false"),
	root("/usr/sbin/systemsetup", "-f", "-setremotelogin", "off"),
}

type fixture struct {
	srv      *stage1test.Server
	b        *Bootstrapper
	agent    []byte
	mu       sync.Mutex
	evs      []macvm.PrepareEvent
	replaced []int // len(Execs()) at each Replaced call
}

func newFixture(t *testing.T, password string) *fixture {
	t.Helper()
	agent := []byte("\xcf\xfa\xed\xfe fake darwin/arm64 wardyn-guest-agent")
	p := filepath.Join(t.TempDir(), "wardyn-guest-agent")
	if err := os.WriteFile(p, agent, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fixture{
		srv: stage1test.New(t, admin, password), agent: agent,
		b: New(Config{AgentPath: p, AgentVersion: "0.10.0-test", OffWait: 200 * time.Millisecond}),
	}
}

func (f *fixture) input(withCode bool) macvm.BootstrapInput {
	in := macvm.BootstrapInput{Dial: f.srv.Dial, Username: admin, Password: provPass, Progress: func(ev macvm.PrepareEvent) {
		f.mu.Lock()
		f.evs = append(f.evs, ev)
		f.mu.Unlock()
	}, GuestEpoch: func() (uint64, error) { return 7, nil }}
	if withCode {
		in.Password, in.NewPassword = code, newPass
		in.Replaced = func() {
			f.mu.Lock()
			f.replaced = append(f.replaced, len(f.srv.Execs()))
			f.mu.Unlock()
		}
	}
	return in
}

func (f *fixture) events() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, ev := range f.evs {
		fmt.Fprintf(&b, "%+v\n", ev)
	}
	return b.String()
}

func argvs(execs []stage1test.Exec) [][]string {
	out := make([][]string, len(execs))
	for i, e := range execs {
		out[i] = e.Argv
	}
	return out
}

func TestSequenceAndArgvExactly(t *testing.T) {
	f := newFixture(t, provPass)
	res, err := f.b.Bootstrap(context.Background(), f.input(false))
	if err != nil {
		t.Fatal(err)
	}
	execs := f.srv.Execs()
	if got := argvs(execs); !slices.EqualFunc(got, wantArgv, slices.Equal[[]string]) {
		t.Fatalf("argv sequence:\n got %q\nwant %q", got, wantArgv)
	}
	for _, e := range execs {
		if !bytes.HasPrefix(e.Stdin, []byte(provPass+"\n")) {
			t.Fatalf("%q: stdin does not open with sudo's password line", e.Line)
		}
	}
	if !bytes.Equal(f.srv.File(guestagent.MacAgentBinaryPath), f.agent) {
		t.Fatal("the guest agent written is not the file sent")
	}
	plist := string(f.srv.File(guestagent.MacAgentPlistPath))
	if !strings.Contains(plist, "<string>"+guestagent.MacAgentLabel+"</string>") || !strings.Contains(plist, "<string>"+guestagent.MacAgentBinaryPath+"</string>") {
		t.Fatalf("plist does not name the shared label and binary:\n%s", plist)
	}
	// sysadminctl reads the agent user's password as stdin's second line;
	// /etc/kcpassword holds the same password, obfuscated.
	i := slices.IndexFunc(execs, func(e stage1test.Exec) bool { return slices.Contains(e.Argv, "-addUser") })
	lines := strings.Split(string(execs[i].Stdin), "\n")
	if len(lines) != 3 || len(lines[1]) != 43 {
		t.Fatalf("agent user's stdin is not sudo's line and a 32-byte password: %d lines", len(lines))
	}
	if !bytes.Equal(f.srv.File("/etc/kcpassword"), kcpassword([]byte(lines[1]))) {
		t.Fatal("kcpassword does not hold the agent user's password")
	}
	if d := f.srv.Dials(); slices.ContainsFunc(d, func(p int) bool { return p != 22 && p != 5900 }) {
		t.Fatalf("dialled ports %v; stage 1 dials the guest's ports 22 and 5900 only", d)
	}
	if res.AgentVersion != "0.10.0-test" || f.srv.RemoteLogin() {
		t.Fatalf("result %+v, Remote Login on %v", res, f.srv.RemoteLogin())
	}
}

func TestEveryStepEmitsAnEvent(t *testing.T) {
	f := newFixture(t, code)
	if _, err := f.b.Bootstrap(context.Background(), f.input(true)); err != nil {
		t.Fatal(err)
	}
	ev := f.events()
	for _, want := range []string{"trusted for this boot only", "replacing the one-time code", "one-time code replaced",
		"installing the guest agent", "creating the agent user", "starting the guest agent", "turning on Screen Sharing",
		"setting power, screen lock and updates", "turning Remote Login off", "Remote Login is off"} {
		if !strings.Contains(ev, want) {
			t.Errorf("no event %q in\n%s", want, ev)
		}
	}
	for _, e := range f.evs {
		if e.Phase != macvm.PhaseBootstrap {
			t.Errorf("event %+v is not in the bootstrap phase", e)
		}
	}
}

func TestD1cReplacesTheCodeFirstAndCallsReplaced(t *testing.T) {
	f := newFixture(t, code)
	if _, err := f.b.Bootstrap(context.Background(), f.input(true)); err != nil {
		t.Fatal(err)
	}
	execs := f.srv.Execs()
	if !slices.Equal(execs[0].Argv, []string{"/usr/bin/dscl", "."}) ||
		string(execs[0].Stdin) != "passwd /Users/wardyn-admin "+code+" "+newPass+"\n" {
		t.Fatalf("first command %q with stdin %q; want the code replaced", execs[0].Line, execs[0].Stdin)
	}
	if !slices.Equal(f.replaced, []int{1}) {
		t.Fatalf("Replaced calls at exec counts %v; want once, right after the replacement", f.replaced)
	}
	if f.srv.Password() != newPass {
		t.Fatal("the guest's password is not the replacement")
	}
	if got := argvs(execs[1:]); !slices.EqualFunc(got, wantArgv, slices.Equal[[]string]) {
		t.Fatalf("after the replacement:\n got %q\nwant %q", got, wantArgv)
	}
	for _, e := range execs[1:] {
		if !bytes.HasPrefix(e.Stdin, []byte(newPass+"\n")) {
			t.Fatalf("%q: sudo is not given the replacement password", e.Line)
		}
	}
}

// secretsIn names every secret value found in text.
func secretsIn(text string, secrets ...string) []string {
	var found []string
	for _, s := range secrets {
		if strings.Contains(text, s) {
			found = append(found, s)
		}
	}
	return found
}

func TestNoSecretInArgvEventsOrErrors(t *testing.T) {
	for _, withCode := range []bool{false, true} {
		for i := -1; i < len(commands); i++ {
			t.Run(fmt.Sprintf("code=%v/fail=%d", withCode, i), func(t *testing.T) {
				if i >= 0 && commands[i].OnlyWithCode && !withCode {
					t.Skip("D1(c) only")
				}
				f := newFixture(t, map[bool]string{false: provPass, true: code}[withCode])
				if i >= 0 {
					f.srv.Fail = func(argv []string) bool { return slices.Equal(argv, commands[i].Argv) }
				}
				_, err := f.b.Bootstrap(context.Background(), f.input(withCode))
				if (err != nil) != (i >= 0) {
					t.Fatalf("err = %v", err)
				}
				var text strings.Builder
				for _, e := range f.srv.Execs() {
					text.WriteString(e.Line + "\n")
				}
				text.WriteString(f.events())
				if err != nil {
					text.WriteString(err.Error())
				}
				agentPw := ""
				for _, e := range f.srv.Execs() {
					if slices.Contains(e.Argv, "-addUser") {
						agentPw = strings.Split(string(e.Stdin), "\n")[1]
					}
				}
				secrets := []string{provPass, code, newPass}
				if agentPw != "" {
					secrets = append(secrets, agentPw)
				}
				if found := secretsIn(text.String(), secrets...); len(found) > 0 {
					t.Fatalf("secrets %q in argv, events or error:\n%s", found, text.String())
				}
			})
		}
	}
}

func TestAFailureAtEachStepStopsStage1(t *testing.T) {
	for i, c := range commands {
		t.Run(c.ID, func(t *testing.T) {
			f := newFixture(t, code)
			f.srv.Fail = func(argv []string) bool { return slices.Equal(argv, c.Argv) }
			_, err := f.b.Bootstrap(context.Background(), f.input(true))
			var se *StepError
			if !errors.As(err, &se) || se.Step != c.Step || se.Command != c.ID {
				t.Fatalf("err = %v; want step %s (%s)", err, c.Step, c.ID)
			}
			execs := f.srv.Execs()
			if len(execs) != i+1 || !slices.Equal(execs[len(execs)-1].Argv, c.argv()) {
				t.Fatalf("%d commands ran, the last %q; want the failing one last", len(execs), execs[len(execs)-1].Line)
			}
			if !f.srv.RemoteLogin() {
				t.Fatal("Remote Login went off although stage 1 failed before step e finished")
			}
		})
	}
}

func TestRemoteLoginOffIsLastAndConfirmed(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		f := newFixture(t, provPass)
		if _, err := f.b.Bootstrap(context.Background(), f.input(false)); err != nil {
			t.Fatal(err)
		}
		execs := f.srv.Execs()
		if last := execs[len(execs)-1].Argv; !slices.Equal(last, root("/usr/sbin/systemsetup", "-f", "-setremotelogin", "off")) {
			t.Fatalf("last command %q; want Remote Login off", last)
		}
		// The session; Screen Sharing and port 22 just before Remote Login
		// off; then port 22 refused and Screen Sharing still answering.
		if d := f.srv.Dials(); !slices.Equal(d, []int{22, 5900, 22, 22, 5900}) {
			t.Fatalf("dials %v", d)
		}
	})
	t.Run("the session ends as it goes off", func(t *testing.T) {
		f := newFixture(t, provPass)
		f.srv.DropOnOff = true
		if _, err := f.b.Bootstrap(context.Background(), f.input(false)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("still answering", func(t *testing.T) {
		f := newFixture(t, provPass)
		f.srv.StaysOn = true
		_, err := f.b.Bootstrap(context.Background(), f.input(false))
		if !errors.Is(err, ErrRemoteLoginStillOn) {
			t.Fatalf("err = %v; want ErrRemoteLoginStillOn", err)
		}
		if strings.Contains(f.events(), "Remote Login is off") {
			t.Fatal("reported off although the guest still answers")
		}
	})
}

func TestHostKeyIsPinnedForTheSession(t *testing.T) {
	f := newFixture(t, code)
	first := f.srv.NewHostKey(t)
	in := f.input(true)
	dials := 0
	in.Dial = func(ctx context.Context, port int) (net.Conn, error) {
		if dials++; dials == 2 {
			f.srv.NewHostKey(t) // the key changes before stage 1 signs in again
		}
		return f.srv.Dial(ctx, port)
	}
	_, err := f.b.Bootstrap(context.Background(), in)
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("err = %v; want ErrHostKeyChanged", err)
	}
	if n := len(f.srv.Execs()); n != 1 || len(f.replaced) != 0 {
		t.Fatalf("%d commands ran, Replaced called %d times; want none after the key changed", n, len(f.replaced))
	}
	if ev := f.events(); !strings.Contains(ev, ssh.FingerprintSHA256(first)) {
		t.Fatalf("the trusted key's fingerprint is not in the events:\n%s", ev)
	}
}

func TestUnsafePasswordIsRefusedBeforeAnyDial(t *testing.T) {
	f := newFixture(t, provPass)
	in := f.input(true)
	in.NewPassword = "two words"
	if _, err := f.b.Bootstrap(context.Background(), in); !errors.Is(err, ErrUnsafePassword) {
		t.Fatalf("err = %v; want ErrUnsafePassword", err)
	}
	if strings.Contains(fmt.Sprint(f.srv.Dials()), "22") {
		t.Fatal("dialled the guest with an unsafe password")
	}
}

func TestCommandTableCitesItsStep(t *testing.T) {
	for _, c := range commands {
		if !slices.Contains(steps, c.Step) || c.ID == "" || len(c.Argv) == 0 {
			t.Errorf("%+v: every row names its §3.4 step, an id and an argv", c)
		}
	}
}

func TestKeyboardInteractiveSignIn(t *testing.T) {
	f := newFixture(t, provPass)
	f.srv.KeyboardOnly = true
	if _, err := f.b.Bootstrap(context.Background(), f.input(false)); err != nil {
		t.Fatal(err)
	}
}

// Interactive dscl exits 0 when its passwd fails: the code file stays until
// the new password has signed in, and stage 1 stops at the rotation.
func TestD1cRotationThatFailsWithExitZeroKeepsTheCode(t *testing.T) {
	f := newFixture(t, code)
	f.srv.RejectPasswd = true
	_, err := f.b.Bootstrap(context.Background(), f.input(true))
	var se *StepError
	if !errors.As(err, &se) || se.Command != "rotate-admin-password" {
		t.Fatalf("err = %v; want the rotation's step error", err)
	}
	if len(f.replaced) != 0 || f.srv.Password() != code || len(f.srv.Execs()) != 1 {
		t.Fatalf("Replaced %d times, guest password replaced %v, %d commands", len(f.replaced), f.srv.Password() != code, len(f.srv.Execs()))
	}
	if found := secretsIn(err.Error()+f.events(), code, newPass); len(found) > 0 {
		t.Fatalf("secrets %q in the error or events", found)
	}
}

// Only a refused dial proves Remote Login off: a dial that hangs, or a relay
// that can no longer reach the guest, fails stage 1.
func TestConfirmOffNeedsARefusal(t *testing.T) {
	for name, dial := range map[string]func(ctx context.Context) (net.Conn, error){
		"hanging dial": func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() },
		"not prep":     func(context.Context) (net.Conn, error) { return nil, vmnet.ErrNotPrep },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, provPass)
			in := f.input(false)
			in.Dial = func(ctx context.Context, port int) (net.Conn, error) {
				if !f.srv.RemoteLogin() {
					return dial(ctx)
				}
				return f.srv.Dial(ctx, port)
			}
			if _, err := f.b.Bootstrap(context.Background(), in); !errors.Is(err, ErrRemoteLoginStillOn) {
				t.Fatalf("err = %v; want ErrRemoteLoginStillOn", err)
			}
		})
	}
}

func TestRefusedIsOnlyARefusal(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{stage1test.Refused(), true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{context.DeadlineExceeded, false},
		{vmnet.ErrNotPrep, false},
		{&net.OpError{Op: "connect", Net: "tcp", Err: errors.New("connection timed out")}, false},
		{nil, false},
	} {
		if got := refused(c.err); got != c.want {
			t.Errorf("refused(%v) = %v; want %v", c.err, got, c.want)
		}
	}
}

// A refused port 22 proves Remote Login off only from a guest shown up, on
// the same boot, just before and just after it; anything else is unknown and
// stops stage 1.
func TestARefusalCountsOnlyFromALiveGuest(t *testing.T) {
	for name, c := range map[string]struct {
		dial  func(ctx context.Context, port int) (net.Conn, error) // after Remote Login off
		epoch func(off bool) (uint64, error)
	}{
		// Port 22 refused, and port 5900 refused too.
		"Screen Sharing silent": {dial: func(context.Context, int) (net.Conn, error) { return nil, stage1test.Refused() }},
		"lease asked again": {epoch: func(off bool) (uint64, error) {
			if off {
				return 8, nil
			}
			return 7, nil
		}},
		"VM stopped": {epoch: func(off bool) (uint64, error) {
			if off {
				return 0, macvm.ErrGuestStopped
			}
			return 7, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, provPass)
			in := f.input(false)
			if c.dial != nil {
				in.Dial = func(ctx context.Context, port int) (net.Conn, error) {
					if !f.srv.RemoteLogin() {
						return c.dial(ctx, port)
					}
					return f.srv.Dial(ctx, port)
				}
			}
			if c.epoch != nil {
				in.GuestEpoch = func() (uint64, error) { return c.epoch(!f.srv.RemoteLogin()) }
			}
			_, err := f.b.Bootstrap(context.Background(), in)
			if !errors.Is(err, ErrGuestLivenessUnknown) {
				t.Fatalf("err = %v; want ErrGuestLivenessUnknown", err)
			}
			if strings.Contains(f.events(), "Remote Login is off") {
				t.Fatal("reported off from a guest not shown to be up")
			}
		})
	}
}

// Before Remote Login off the guest must be shown up: with Screen Sharing
// silent, or no epoch to read, stage 1 stops and never turns it off.
func TestBeforeOffTheGuestMustBeShownUp(t *testing.T) {
	for name, edit := range map[string]func(*fixture, *macvm.BootstrapInput){
		"Screen Sharing silent": func(f *fixture, in *macvm.BootstrapInput) {
			in.Dial = func(ctx context.Context, port int) (net.Conn, error) {
				if port == 5900 {
					return nil, stage1test.Refused()
				}
				return f.srv.Dial(ctx, port)
			}
		},
		"no epoch": func(_ *fixture, in *macvm.BootstrapInput) { in.GuestEpoch = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, provPass)
			in := f.input(false)
			edit(f, &in)
			if _, err := f.b.Bootstrap(context.Background(), in); !errors.Is(err, ErrGuestLivenessUnknown) {
				t.Fatalf("err = %v; want ErrGuestLivenessUnknown", err)
			}
			execs := f.srv.Execs()
			if !f.srv.RemoteLogin() || slices.Equal(execs[len(execs)-1].Argv, root("/usr/sbin/systemsetup", "-f", "-setremotelogin", "off")) {
				t.Fatal("Remote Login was turned off from a guest not shown to be up")
			}
		})
	}
}
