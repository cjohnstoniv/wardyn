// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records the commands the darwin code runs and answers from out.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   func(argv []string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	if f.out != nil {
		return f.out(argv)
	}
	return nil, nil
}

type fakeConsole struct {
	uid uint32
	err error
}

func (f fakeConsole) Console(context.Context) (uint32, error) { return f.uid, f.err }

func TestCAEnvReplacingVariablesNameTheFullBundle(t *testing.T) {
	env := CAEnv(MacCABundlePath, MacCAFullPath)
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "AWS_CA_BUNDLE", "GIT_SSL_CAINFO"} {
		if !slices.Contains(env, k+"=/Users/agent/.wardyn/ca-bundle.pem") {
			t.Errorf("%s is not the full bundle: %v", k, env)
		}
	}
	if !slices.Contains(env, "NODE_EXTRA_CA_CERTS=/Users/agent/.wardyn/ca.pem") || len(env) != 6 {
		t.Errorf("NODE_EXTRA_CA_CERTS must add the CA alone: %v", env)
	}
}

func TestCAEnvForNeedsTheBundleFile(t *testing.T) {
	if got := caEnvFor([]File{{Path: "/etc/other"}}); got != nil {
		t.Errorf("another file set CA variables: %v", got)
	}
	if got := caEnvFor([]File{{Path: "/etc/other"}, {Path: MacCABundlePath}}); len(got) != 6 {
		t.Errorf("the bundle file set %d variables, want 6", len(got))
	}
}

// The dispatcher puts Linux paths for these same variables in Init's Env: the agent's must win.
func TestInitEnvCAVariablesWinOverInit(t *testing.T) {
	ca := CAEnv(MacCABundlePath, MacCAFullPath)
	env := map[string]string{"SSL_CERT_FILE": "/tmp/wardyn/ca-bundle.pem", "NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem", "KEEP": "1"}
	secret := map[string]string{"GIT_SSL_CAINFO": "/tmp/wardyn/x", "TOKEN": "t"}
	got := initEnv(env, secret, ca)
	for _, kv := range got {
		if strings.Contains(kv, "/tmp/wardyn") {
			t.Errorf("Init's CA path survived: %s", kv)
		}
	}
	for _, want := range append([]string{"KEEP=1", "TOKEN=t"}, ca...) {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	// Without a run CA nothing is overridden.
	if got := initEnv(env, nil, nil); !slices.Contains(got, "SSL_CERT_FILE=/tmp/wardyn/ca-bundle.pem") {
		t.Errorf("Init's variable was dropped without a CA: %v", got)
	}
}

func TestLookupAgentIDs(t *testing.T) {
	r := &fakeRunner{out: func(a []string) ([]byte, error) {
		return []byte(map[string]string{"-u": "502\n", "-g": "20\n", "-G": "20 12 61 502\n"}[a[1]]), nil
	}}
	ids, err := LookupAgentIDs(context.Background(), r, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if ids.UID != 502 || ids.GID != 20 || !slices.Equal(ids.Groups, []uint32{12, 61, 502}) {
		t.Errorf("ids = %+v", ids)
	}
	if r.calls[0][0] != "/usr/bin/id" || r.calls[0][2] != "agent" {
		t.Errorf("first call = %v", r.calls[0])
	}
	bad := &fakeRunner{out: func([]string) ([]byte, error) { return []byte("nope"), nil }}
	if _, err := LookupAgentIDs(context.Background(), bad, "agent"); err == nil {
		t.Error("garbage from id was accepted")
	}
	gone := &fakeRunner{out: func([]string) ([]byte, error) { return nil, errors.New("no such user") }}
	if _, err := LookupAgentIDs(context.Background(), gone, "agent"); err == nil {
		t.Error("a missing account was accepted")
	}
}

func TestLookupAgentIDsRefusesRootAndAdmins(t *testing.T) {
	for name, ids := range map[string][3]string{
		"root":          {"0", "20", "20"},
		"admin primary": {"502", "80", "80"},
		"admin member":  {"502", "20", "20 80 61"},
	} {
		r := &fakeRunner{out: func(a []string) ([]byte, error) {
			return []byte(map[string]string{"-u": ids[0], "-g": ids[1], "-G": ids[2]}[a[1]]), nil
		}}
		if _, err := LookupAgentIDs(context.Background(), r, "agent"); !errors.Is(err, ErrAgentPrivileged) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// vncSettings is a Desktop whose settings file is in a test directory.
func vncSettings(t *testing.T, r CmdRunner) *Desktop {
	return &Desktop{Run: r, VNCSettings: filepath.Join(t.TempDir(), "com.apple.VNCSettings.txt")}
}

// noArgvHolds fails if any command the agent ran carries pw: any guest process can read another's
// arguments with ps.
func noArgvHolds(t *testing.T, r *fakeRunner, pw string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, argv := range r.calls {
		if strings.Contains(strings.Join(argv, "\x00"), pw) {
			t.Errorf("a command carries the password: %v", argv[0])
		}
	}
}

func TestSetRunPasswordWritesTheSettingsFileAndHoldsIt(t *testing.T) {
	r := &fakeRunner{}
	d := vncSettings(t, r)
	if _, ok := d.Password(); ok {
		t.Fatal("a password before SetRunPassword")
	}
	if err := d.SetRunPassword(context.Background(), "Abcd2345"); err != nil {
		t.Fatal(err)
	}
	noArgvHolds(t, r, "Abcd2345")
	got, err := os.ReadFile(d.VNCSettings)
	if err != nil || string(got) != "5656320AB99BF1D7FF1C39567390ADCA\n" {
		t.Errorf("settings file = %q, %v", got, err)
	}
	if fi, err := os.Lstat(d.VNCSettings); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings file = %v; want a 0600 file", err)
	}
	if pw, ok := d.Password(); !ok || pw != "Abcd2345" {
		t.Errorf("Password() = %q, %v", pw, ok)
	}
	// Setting the password runs no command at all.
	if len(r.calls) != 0 {
		t.Errorf("setting the password ran %v", r.calls)
	}
}

func TestSetRunPasswordFailureKeepsNoPasswordAndLeaksNone(t *testing.T) {
	d := &Desktop{Run: &fakeRunner{}, VNCSettings: filepath.Join(t.TempDir(), "missing", "com.apple.VNCSettings.txt")}
	err := d.SetRunPassword(context.Background(), "Abcd2345")
	if err == nil {
		t.Fatal("the settings write failed and SetRunPassword did not")
	}
	if strings.Contains(err.Error(), "Abcd2345") {
		t.Errorf("the error holds the password: %v", err)
	}
	if _, ok := d.Password(); ok {
		t.Error("a failed set left a password")
	}
}

func TestPointerCommandRunsInTheConsoleUsersSession(t *testing.T) {
	name, args := PointerCommand(502)
	want := []string{"asuser", "502", "/usr/bin/sudo", "-u", "#502", "/usr/bin/osascript", "-l", "JavaScript", "-e", pointerScript}
	if name != "/bin/launchctl" || !slices.Equal(args, want) {
		t.Errorf("command = %s %v\nwant launchctl %v", name, args, want)
	}
}

func TestPointerReadsThroughTheConsoleUser(t *testing.T) {
	r := &fakeRunner{out: func([]string) ([]byte, error) { return []byte("960 540\n"), nil }}
	d := &Desktop{Run: r, Console: fakeConsole{uid: 502}}
	p, err := d.Pointer(context.Background())
	if err != nil || p != (Pointer{X: 960, Y: 540}) {
		t.Fatalf("Pointer = %+v, %v", p, err)
	}
	if got := r.calls[0]; got[0] != "/bin/launchctl" || got[1] != "asuser" || got[2] != "502" {
		t.Errorf("the read did not run in uid 502's session: %v", got)
	}
}

func TestPointerWithNobodyLoggedIn(t *testing.T) {
	r := &fakeRunner{}
	d := &Desktop{Run: r, Console: fakeConsole{err: ErrNoConsoleUser}}
	if _, err := d.Pointer(context.Background()); !errors.Is(err, ErrNoConsoleUser) {
		t.Errorf("err = %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("a pointer read ran with nobody at the console: %v", r.calls)
	}
}

func TestParsePointer(t *testing.T) {
	for in, want := range map[string]Pointer{"10 20": {10, 20}, " 10.4 20.6\n": {10, 21}, "-3 0": {-3, 0}} {
		if got, err := parsePointer(in); err != nil || got != want {
			t.Errorf("parsePointer(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1", "1 2 3", "a b", "NaN 1", "1e12 1"} {
		if _, err := parsePointer(in); err == nil {
			t.Errorf("parsePointer(%q) was accepted", in)
		}
	}
}

func TestSetSessionEnvCommandLines(t *testing.T) {
	r := &fakeRunner{}
	d := &Desktop{Run: r, Console: fakeConsole{uid: 502}}
	if err := d.SetSessionEnv(context.Background(), []string{"SSL_CERT_FILE=/x", "A=b=c"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/bin/launchctl", "asuser", "502", "/bin/launchctl", "setenv", "SSL_CERT_FILE", "/x"},
		{"/bin/launchctl", "asuser", "502", "/bin/launchctl", "setenv", "A", "b=c"},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Errorf("calls = %v", r.calls)
	}
	none := &Desktop{Run: &fakeRunner{}, Console: fakeConsole{err: ErrNoConsoleUser}}
	if err := none.SetSessionEnv(context.Background(), nil); err != nil {
		t.Errorf("no variables still asked for a console user: %v", err)
	}
}

func TestDesktopRequests(t *testing.T) {
	r := &fakeRunner{out: func(a []string) ([]byte, error) {
		if a[0] == "/bin/launchctl" {
			return []byte("100 200"), nil
		}
		return nil, nil
	}}
	d := vncSettings(t, r)
	d.Console = fakeConsole{uid: 502}
	mux := (&Tools{Desktop: d}).ToolsHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if err := d.SetRunPassword(context.Background(), "Abcd2345"); err != nil {
		t.Fatal(err)
	}
	// The password is the host's: the agent never serves it back.
	if w := get("/v1/desktop/credential"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "Abcd2345") {
		t.Errorf("credential = %d %q, want no such request", w.Code, w.Body.String())
	}
	if w := get("/v1/tools/pointer"); w.Code != 200 || !strings.Contains(w.Body.String(), `"x":100`) {
		t.Errorf("pointer = %d %q", w.Code, w.Body.String())
	}
	d.Console = fakeConsole{err: ErrNoConsoleUser}
	if w := get("/v1/tools/pointer"); w.Code != http.StatusConflict {
		t.Errorf("pointer with nobody at the console = %d", w.Code)
	}
}

// agentBroker stands in for the helper: each dial the agent makes gets one end of a pipe, and the
// test takes the other.
type agentBroker struct {
	mu    sync.Mutex
	dials int
	hosts chan net.Conn
}

func newBroker() *agentBroker { return &agentBroker{hosts: make(chan net.Conn, 8)} }

func (b *agentBroker) dial(context.Context) (net.Conn, error) {
	guest, host := net.Pipe()
	b.mu.Lock()
	b.dials++
	b.mu.Unlock()
	b.hosts <- host
	return guest, nil
}

func (b *agentBroker) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-b.hosts:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not dial")
		return nil
	}
}

func TestServeVsockServesTheClientAndRedials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) { writeJSONAny(w, Ready{Version: "mac-test"}) })
	b := newBroker()
	done := make(chan struct{})
	go func() { defer close(done); _ = serveVsock(ctx, b.dial, mux, 10*time.Millisecond) }()

	for session := range 2 {
		host := b.next(t)
		cl := NewClient(func(context.Context) (net.Conn, error) { return host, nil })
		got, err := cl.Ready(ctx)
		if err != nil || got.Version != "mac-test" {
			t.Fatalf("session %d: Ready = %+v, %v", session, got, err)
		}
		cl.Close() // the helper closing the pair: the agent dials again
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ServeVsock did not stop with its context")
	}
	if b.dials < 2 {
		t.Errorf("the agent dialled %d times, want a redial after the first session", b.dials)
	}
}

func TestServeVsockIdleUntilTheHostSaysHello(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newBroker()
	go func() { _ = serveVsock(ctx, b.dial, http.NewServeMux(), 10*time.Millisecond) }()
	host := b.next(t)
	host.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := host.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("the idle agent wrote %d bytes before a hello (%v)", n, err)
	}
}

func allowAll(io.Closer) (func(), bool) { return func() {}, true }

func TestSpliceDesktopRefusesWhenTheDesktopIsOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newBroker()
	lb := newBroker()
	d := &Desktop{}
	d.Disable()
	go func() { _ = spliceDesktop(ctx, hb.dial, lb.dial, d.Track, 10*time.Millisecond) }()
	host := hb.next(t)
	_, _ = host.Write([]byte{DesktopOpenByte})
	select {
	case <-lb.hosts:
		t.Fatal("Screen Sharing was dialled for a run whose desktop is off")
	case <-time.After(200 * time.Millisecond):
	}
	hb.next(t) // refused: the agent closed it and dialled again
}

func TestDisableClosesAnOpenTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newBroker()
	lb := newBroker()
	d := &Desktop{}
	go func() { _ = spliceDesktop(ctx, hb.dial, lb.dial, d.Track, 10*time.Millisecond) }()
	host := hb.next(t)
	_, _ = host.Write([]byte{DesktopOpenByte})
	local := lb.next(t)
	go local.Write([]byte("x"))
	if _, err := io.ReadFull(host, make([]byte, 1)); err != nil {
		t.Fatal(err) // the tunnel carries traffic
	}
	// The tunnel is tracked just after the dial; Disable must reach it whenever that is.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		n := len(d.open)
		d.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	d.Disable()
	local.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := local.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Screen Sharing's side stayed open after Disable: %v", err)
	}
}

func TestSpliceDesktopWaitsForTheOpenByte(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newBroker()
	lb := newBroker()
	go func() { _ = spliceDesktop(ctx, hb.dial, lb.dial, allowAll, 10*time.Millisecond) }()

	host := hb.next(t)
	select {
	case <-lb.hosts:
		t.Fatal("Screen Sharing was dialled before a viewer came")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := host.Write([]byte{DesktopOpenByte}); err != nil {
		t.Fatal(err)
	}
	local := lb.next(t)
	go local.Write([]byte("RFB 003.889\n"))
	got := make([]byte, 12)
	if _, err := io.ReadFull(host, got); err != nil || string(got) != "RFB 003.889\n" {
		t.Fatalf("server to host = %q, %v", got, err)
	}
	go host.Write([]byte("RFB 003.008\n"))
	if _, err := io.ReadFull(local, got); err != nil || string(got) != "RFB 003.008\n" {
		t.Fatalf("host to server = %q, %v", got, err)
	}
	local.Close() // Screen Sharing ends: the host side is released and the agent dials again
	host.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := host.Read(got); err == nil {
		t.Error("the host side stayed open after Screen Sharing closed")
	}
	hb.next(t)
}

func TestSpliceDesktopRefusesAnotherFirstByte(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hb := newBroker()
	lb := newBroker()
	go func() { _ = spliceDesktop(ctx, hb.dial, lb.dial, allowAll, 10*time.Millisecond) }()
	host := hb.next(t)
	_, _ = host.Write([]byte{'R'})
	select {
	case <-lb.hosts:
		t.Fatal("a first byte that is not the open byte reached Screen Sharing")
	case <-time.After(200 * time.Millisecond):
	}
	hb.next(t) // the agent closed it and dialled again
}

// The password comes from Init, so SetRunPassword takes only one VncAuth can key on: 1 to 8 bytes.
func TestSetRunPasswordRefusesWhatVncAuthCannotUse(t *testing.T) {
	r := &fakeRunner{}
	d := vncSettings(t, r)
	for _, pw := range []string{"", "Abcd23456"} {
		if err := d.SetRunPassword(context.Background(), pw); err == nil || (pw != "" && strings.Contains(err.Error(), pw)) {
			t.Errorf("password of %d bytes: err = %v", len(pw), err)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("a command ran: %v", r.calls)
	}
	if _, err := os.Stat(d.VNCSettings); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused password was written: %v", err)
	}
	if _, ok := d.Password(); ok {
		t.Error("a refused password was kept")
	}
}

// Init's secrets never print: %v, %+v, %#v and %s show them redacted, and the wire form keeps them.
func TestInitPrintsWithItsSecretsRedacted(t *testing.T) {
	in := Init{Env: map[string]string{"A": "visible"}, SecretEnv: map[string]string{"TOKEN": "s3cret-env"}, DesktopPassword: "Abcd2345"}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(f, in)
		if strings.Contains(out, "Abcd2345") || strings.Contains(out, "s3cret-env") || !strings.Contains(out, "visible") || !strings.Contains(out, "TOKEN") {
			t.Errorf("%s printed %q", f, out)
		}
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("init", "in", in)
	if strings.Contains(logged.String(), "Abcd2345") || strings.Contains(logged.String(), "s3cret-env") {
		t.Errorf("slog's JSON handler logged %s", logged.String())
	}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"desktop_password":"Abcd2345"`) {
		t.Errorf("the wire form lost the password: %s", b)
	}
	if in.DesktopPassword != "Abcd2345" || in.SecretEnv["TOKEN"] != "s3cret-env" {
		t.Error("printing changed the Init")
	}
}
