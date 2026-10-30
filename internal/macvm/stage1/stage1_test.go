// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package stage1

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// pairKey is the base's pairing key, printable so a leak of it shows in any text it reaches.
var pairKey = []byte("wardyn-test-pair-key-0123456789!")

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
	root("/usr/bin/install", "-d", "-o", "root", "-g", "wheel", "-m", "755", "/Library/Wardyn"),
	root("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "600", "/dev/null", "/Library/Wardyn/pair.key"),
	root("/bin/dd", "of=/Library/Wardyn/pair.key"),
	root("/usr/sbin/sysadminctl", "-addUser", "agent", "-fullName", "agent", "-password", "-"),
	root("/usr/bin/dscl", ".", "-list", "/Users", "UniqueID"),
	root("/usr/bin/defaults", "write", "/Library/Preferences/com.apple.loginwindow", "autoLoginUser", "agent"),
	root("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "600", "/dev/null", "/etc/kcpassword"),
	root("/bin/dd", "of=/etc/kcpassword"),
	root("/bin/launchctl", "enable", "system/com.wardyn.guest-agent"),
	root("/bin/launchctl", "bootstrap", "system", "/Library/LaunchDaemons/com.wardyn.guest-agent.plist"),
	root("/bin/launchctl", "enable", "system/com.apple.screensharing"),
	root("/bin/launchctl", "print", "system/com.apple.screensharing"),
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
	}, GuestEpoch: func() (uint64, error) { return 7, nil }, PairKey: bytes.Clone(pairKey)}
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
	// The pairing key reaches the guest's root-only file over dd's stdin, after sudo's line, and no argv.
	if guestagent.MacPairKeyPath != "/Library/Wardyn/pair.key" {
		t.Fatalf("stage 1 writes the key where the guest agent does not read it: %s", guestagent.MacPairKeyPath)
	}
	if !bytes.Equal(f.srv.File("/Library/Wardyn/pair.key"), pairKey) {
		t.Fatal("the guest's pairing key is not the key prepare gave stage 1")
	}
	k := slices.IndexFunc(execs, func(e stage1test.Exec) bool {
		return slices.Equal(e.Argv, root("/bin/dd", "of=/Library/Wardyn/pair.key"))
	})
	if k < 0 || !bytes.Equal(execs[k].Stdin, append([]byte(provPass+"\n"), pairKey...)) {
		t.Fatal("the key's dd does not read sudo's line and then the key alone")
	}
	for _, e := range execs {
		if strings.Contains(e.Line, string(pairKey)) || slices.ContainsFunc(e.Argv, func(w string) bool { return strings.Contains(w, string(pairKey)) }) {
			t.Fatalf("the pairing key is in the argv %q", e.Line)
		}
	}
	if d := f.srv.Dials(); slices.ContainsFunc(d, func(p int) bool { return p != 22 && p != 5900 }) {
		t.Fatalf("dialled ports %v; stage 1 dials the guest's ports 22 and 5900 only", d)
	}
	if res.AgentVersion != "0.10.0-test" || f.srv.RemoteLogin() {
		t.Fatalf("result %+v, Remote Login on %v", res, f.srv.RemoteLogin())
	}
}

// Screen Sharing turned on in System Settings is loaded already, and bootstrapping a loaded service exits 5:
// stage 1 probes first and skips the bootstrap, and the RFB banner check still has to pass.
func TestScreenSharingLoadedByHandIsNotBootstrappedAgain(t *testing.T) {
	f := newFixture(t, provPass)
	f.srv.LoadSharing()
	if _, err := f.b.Bootstrap(context.Background(), f.input(false)); err != nil {
		t.Fatalf("stage 1 with Screen Sharing already loaded: %v", err)
	}
	for _, a := range argvs(f.srv.Execs()) {
		if slices.Contains(a, "bootstrap") && strings.HasSuffix(a[len(a)-1], "com.apple.screensharing.plist") {
			t.Fatalf("bootstrapped a loaded Screen Sharing: %q", a)
		}
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
				secrets := []string{provPass, code, newPass, string(pairKey), hex.EncodeToString(pairKey), base64.StdEncoding.EncodeToString(pairKey)}
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
			probes := 0 // each SkipIfOK row costs one more exec
			for _, p := range commands[:i+1] {
				if p.SkipIfOK != nil {
					probes++
				}
			}
			if len(execs) != i+1+probes || !slices.Equal(execs[len(execs)-1].Argv, c.argv()) {
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

func TestRefusedPasswordIsErrSignInRefused(t *testing.T) {
	f := newFixture(t, provPass)
	in := f.input(false)
	in.Password = "not-the-password"
	_, err := f.b.Bootstrap(context.Background(), in)
	if !errors.Is(err, ErrSignInRefused) || !errors.Is(err, macvm.ErrSignInRefused) {
		t.Fatalf("err = %v; want ErrSignInRefused", err)
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
	if errors.Is(err, ErrSignInRefused) { // a failed rotation is not a wrong account
		t.Fatalf("err = %v; a refused sign-in after the rotation reads as ErrSignInRefused", err)
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
	type hooks struct {
		// dial answers every dial once Remote Login is off (nil: the guest).
		dial func(ctx context.Context, port int) (net.Conn, error)
		// epoch is the guest's epoch once Remote Login is off (nil: 7).
		epoch func() (uint64, error)
	}
	refused22 := func(f *fixture, then func()) func(ctx context.Context, port int) (net.Conn, error) {
		return func(ctx context.Context, port int) (net.Conn, error) {
			if port == 22 {
				if then != nil {
					then()
				}
				return nil, stage1test.Refused()
			}
			return f.srv.Dial(ctx, port)
		}
	}
	for name, setup := range map[string]func(f *fixture) hooks{
		// Port 22 refused, and port 5900 refused too.
		"Screen Sharing refused": func(*fixture) hooks {
			return hooks{dial: func(context.Context, int) (net.Conn, error) { return nil, stage1test.Refused() }}
		},
		// Port 22 refused; port 5900 connects and sends nothing.
		"Screen Sharing mute": func(f *fixture) hooks {
			return hooks{dial: refused22(f, func() { f.srv.MuteRFB(true) })}
		},
		"lease asked again": func(*fixture) hooks {
			return hooks{epoch: func() (uint64, error) { return 8, nil }}
		},
		// Port 22 keeps answering while the lease changes: the check before
		// each dial stops at once, not after OffWait.
		"lease asked again while port 22 still answers": func(f *fixture) hooks {
			f.srv.StaysOn = true
			return hooks{epoch: func() (uint64, error) { return 8, nil }}
		},
		// The lease changes only with the refusal: only the check after it
		// can see that.
		"lease asked again at the refusal": func(f *fixture) hooks {
			var bumps atomic.Uint64
			return hooks{dial: refused22(f, func() { bumps.Add(1) }), epoch: func() (uint64, error) { return 7 + bumps.Load(), nil }}
		},
		"VM stopped": func(*fixture) hooks {
			return hooks{epoch: func() (uint64, error) { return 0, macvm.ErrGuestStopped }}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, provPass)
			h := setup(f)
			in := f.input(false)
			if h.dial != nil {
				in.Dial = func(ctx context.Context, port int) (net.Conn, error) {
					if remoteLoginTurnedOff(f) {
						return h.dial(ctx, port)
					}
					return f.srv.Dial(ctx, port)
				}
			}
			if h.epoch != nil {
				in.GuestEpoch = func() (uint64, error) {
					if remoteLoginTurnedOff(f) {
						return h.epoch()
					}
					return 7, nil
				}
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

// remoteLoginTurnedOff says whether the guest has run Remote Login off.
func remoteLoginTurnedOff(f *fixture) bool {
	execs := f.srv.Execs()
	return len(execs) > 0 && slices.Equal(execs[len(execs)-1].Argv, root("/usr/sbin/systemsetup", "-f", "-setremotelogin", "off"))
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

// Every stdin is built at its exact size: no regrowth leaves a password or
// the code behind in a dropped array.
func TestStdinIsSizedExactly(t *testing.T) {
	agent := make([]byte, 5000)
	s, err := newSession(macvm.BootstrapInput{Username: admin, Password: code, NewPassword: newPass}, agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		if in := s.stdin(c); len(in) != cap(in) {
			t.Errorf("%s: stdin is %d bytes in a %d-byte array", c.ID, len(in), cap(in))
		}
	}
}

// Stage 1 finishes only when the guest's local users of UniqueID 500 or more are exactly the admin and the
// agent; a person's extra account is refused by name, and a listing that cannot be read is no pass.
func TestOnlyTheAdminAndTheAgentMayBeAccounts(t *testing.T) {
	t.Run("exactly the two", func(t *testing.T) {
		f := newFixture(t, code)
		if _, err := f.b.Bootstrap(context.Background(), f.input(true)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an extra account", func(t *testing.T) {
		f := newFixture(t, code)
		f.srv.ExtraAccounts = []string{"intruder"}
		_, err := f.b.Bootstrap(context.Background(), f.input(true))
		var se *StepError
		if !errors.Is(err, ErrUnexpectedAccounts) || !errors.As(err, &se) || se.Command != "accounts-check" || !strings.Contains(err.Error(), "intruder") {
			t.Fatalf("err = %v; want ErrUnexpectedAccounts at accounts-check naming intruder", err)
		}
		if !f.srv.RemoteLogin() {
			t.Fatal("stage 1 went on to turn Remote Login off after the refusal")
		}
	})
	t.Run("the listing cannot be read", func(t *testing.T) {
		s := &session{username: admin}
		for _, out := range []string{"", "wardyn-admin 501 extra\n", "wardyn-admin abc\nagent 502\n", "agent 502\n", "wardyn-admin 501\n"} {
			if err := s.checkAccounts(out); !errors.Is(err, ErrUnexpectedAccounts) {
				t.Errorf("checkAccounts(%q) = %v, want a refusal", out, err)
			}
		}
		if err := s.checkAccounts("root 0\nnobody -2\n_spotlight 89\nwardyn-admin 501\nagent 502\n"); err != nil {
			t.Errorf("the two accounts among the system ones were refused: %v", err)
		}
		if err := s.checkAccounts("_nouid\nwardyn-admin 501\nagent 502\n"); err != nil {
			t.Errorf("a record with no UniqueID was refused: %v", err)
		}
		if err := s.checkAccounts("wardyn-admin\nagent 502\n"); !errors.Is(err, ErrUnexpectedAccounts) {
			t.Errorf("an admin with no UniqueID passed: %v", err)
		}
	})
}

// Stage 1 does not run without the base's pairing key: a base it made could never pair.
func TestStage1RefusesAnInputWithoutAPairKey(t *testing.T) {
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{1}, 31)} {
		f := newFixture(t, provPass)
		in := f.input(false)
		in.PairKey = key
		if _, err := f.b.Bootstrap(context.Background(), in); err == nil {
			t.Fatalf("stage 1 ran with a %d-byte pairing key", len(key))
		}
		if len(f.srv.Execs()) != 0 {
			t.Fatal("stage 1 ran a command before it had a pairing key")
		}
	}
}
