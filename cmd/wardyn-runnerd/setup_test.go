// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// setupFixture is a temporary home, a recording command runner, a fake Docker and an organisation that
// counts what it is asked, so setup runs end to end without touching this computer's services.
type setupFixture struct {
	t           *testing.T
	host        setupHost
	org         string
	tokenFile   string
	mu          sync.Mutex
	calls       []string // "METHOD path" the organisation served
	commands    []string // "name args" run on the host
	networks    []string
	checks      []string    // "image network" of each engine check
	wants       []relayPair // the explicit relay values each check was handed
	engineErr   error       // what the engine check answers, when set
	relayErr    error       // what the relay probe answers, when set
	events      []string    // engine, install, register, probe, service: the order setup did them in
	flagless    bool        // run leaves --proxy-image out
	unpullable  string      // an image the relay probe cannot get onto the engine
	answered    string      // the proxy image the organisation answers a registration with
	desktop     bool
	vm          bool // a VM-backed engine other than Docker Desktop
	state       types.RunnerState
	pools       []sdk.RunnerPool
	defaultID   *uuid.UUID
	id          uuid.UUID
	refuseClaim bool
	otherPool   *sdk.RunnerPool
}

func newSetupFixture(t *testing.T, goos string) *setupFixture {
	t.Helper()
	f := &setupFixture{t: t, id: uuid.New(), state: types.RunnerUnclaimed}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.org = srv.URL
	root := t.TempDir()
	exe := filepath.Join(root, "downloaded-wardyn-runnerd")
	if err := os.WriteFile(exe, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.tokenFile = filepath.Join(root, "token")
	if err := os.WriteFile(f.tokenFile, []byte("wdr_"+strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	f.host = setupHost{goos: goos, home: filepath.Join(root, "home"), uid: 501, exe: exe,
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			f.commands = append(f.commands, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 1 {
				switch args[1] {
				case "stop":
					f.events = append(f.events, "install")
				case "enable":
					f.events = append(f.events, "service")
				}
			}
			if name == "systemctl" && len(args) > 2 && args[1] == "show" {
				return []byte(filepath.Join(f.host.home, ".config", "systemd", "user") + " /etc/systemd/user\n"), nil
			}
			return nil, nil
		},
		engine: f.engine, relay: f.relay}
	if err := os.MkdirAll(f.host.home, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// engine answers as readySetupEngine would on a native engine, on Docker Desktop or on a VM-backed
// engine reached at 192.168.5.2, and applies the real bind rule.
func (f *setupFixture) engine(_ context.Context, network string, want relayPair) (dockerEngine, error) {
	f.events = append(f.events, "engine")
	f.networks = append(f.networks, network)
	if f.engineErr != nil {
		return dockerEngine{}, f.engineErr
	}
	gw := netip.MustParseAddr("172.17.0.1")
	eng := dockerEngine{desc: `"Ubuntu 24.04 LTS" (laptop)`, native: true, gateway: gw, bridge: []netip.Addr{gw}}
	switch {
	case f.desktop:
		eng = dockerEngine{desc: `"Docker Desktop" (docker-desktop)`, why: "Docker Desktop"}
	case f.vm:
		eng = dockerEngine{desc: `"Alpine Linux v3.21" (lima-rancher-desktop)`, why: "its bridge interface is not on this host"}
	}
	if want.bind.IsValid() {
		if err := admitRelayBind(want.bind, eng); err != nil {
			return dockerEngine{}, err
		}
	}
	return eng, nil
}

// relay answers as probeSetupRelay would: the native and Docker Desktop engines' proxies reach the relay
// through host-gateway, a VM-backed engine's at 192.168.5.2, and an explicit target always reaches.
func (f *setupFixture) relay(_ context.Context, image, network string, eng dockerEngine, want relayPair) (relayPair, error) {
	f.events = append(f.events, "probe")
	f.checks = append(f.checks, image+" "+network)
	f.wants = append(f.wants, want)
	if image == f.unpullable {
		return relayPair{}, fmt.Errorf("%w: %s", errProxyImageUnavailable, image)
	}
	if f.relayErr != nil {
		return relayPair{}, f.relayErr
	}
	reach := hostGateway
	if f.vm {
		reach = "192.168.5.2"
	}
	for _, p := range relayCandidates(eng, want) {
		if p.target == reach || p.target == want.target {
			return p, nil
		}
	}
	return relayPair{}, errors.New("relay: no container on Docker engine " + eng.desc + " reached this runner")
}

func (f *setupFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/api/v1/runners/register":
		var req types.RunnerRegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.events = append(f.events, "register")
		w.WriteHeader(http.StatusCreated)
		reply(types.RunnerRegistration{RunnerID: f.id, Fingerprint: runnerwire.Fingerprint(req.PublicKey), State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(f.org), ProxyImage: f.answered})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/me/runners/"+f.id.String():
		reply(types.RunnerView{ID: f.id, Name: "laptop", State: f.state})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/me/runners/"+f.id.String()+"/claim":
		if f.refuseClaim {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.state = types.RunnerClaimed
		reply(types.RunnerRegistration{RunnerID: f.id, State: types.RunnerClaimed})
	case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/runner-pools" || r.URL.Path == "/api/v1/me/runner-pools"):
		// No Slots on a row: the lookup must not read ownership from the slot count, which can fail.
		var entries []sdk.RunnerPoolCatalogueEntry
		for _, p := range f.pools {
			entries = append(entries, sdk.RunnerPoolCatalogueEntry{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: p.ID, Name: p.Name, Scope: p.Scope}})
		}
		if f.otherPool != nil && r.URL.Path == "/api/v1/runner-pools" {
			// Only an administrator's shared list also carries other people's user pools, for oversight.
			entries = append(entries, sdk.RunnerPoolCatalogueEntry{RunnerPoolChoice: sdk.RunnerPoolChoice{ID: f.otherPool.ID, Name: f.otherPool.Name, Scope: sdk.RunnerPoolUser}, Owner: "someone-else"})
		}
		reply(sdk.RunnerPoolList{Pools: entries})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/me/runner-pools":
		var req sdk.CreateMyRunnerPoolRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		p := sdk.RunnerPool{ID: uuid.New(), Name: req.Name, Scope: sdk.RunnerPoolUser}
		f.pools = append(f.pools, p)
		reply(p)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runner-pools/"):
		for _, p := range f.pools {
			if strings.HasSuffix(r.URL.Path, p.ID.String()) {
				reply(p)
			}
		}
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/runners/"+f.id.String()):
		reply(sdk.RunnerPoolMember{})
	case r.URL.Path == "/api/v1/me/runner-pool-defaults":
		if r.Method == http.MethodPut {
			var d types.RunnerPoolDefaults
			_ = json.NewDecoder(r.Body).Decode(&d)
			f.defaultID = d.PoolID
		}
		reply(types.RunnerPoolDefaults{PoolID: f.defaultID})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *setupFixture) count(call string) int {
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *setupFixture) run(o setupOptions) (string, error) {
	f.t.Helper()
	if o.org == "" {
		o.org = f.org
	}
	if o.network == "" {
		o.network = "wardyn-internal"
	}
	if o.proxyImage == "" && !f.flagless {
		o.proxyImage = "ghcr.io/example/proxy:1"
	}
	cmd := newSetupCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	err := runSetup(cmd, f.host, o)
	return out.String(), err
}

func (f *setupFixture) read(path string) string {
	f.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// An unreachable systemd user manager is reported as unreachable, not as one that searches another directory.
func TestCheckServiceManagerNamesAnUnreachableManager(t *testing.T) {
	var p setupPaths
	answer := func(out string, err error) setupHost {
		return setupHost{goos: "linux", home: "/h", run: func(context.Context, string, ...string) ([]byte, error) { return []byte(out), err }}
	}
	if err := answer("Failed to connect to bus\n", errors.New("exit status 1")).checkServiceManager(context.Background(), &p); err == nil || !strings.Contains(err.Error(), "did not answer") || strings.Contains(err.Error(), "does not search") {
		t.Errorf("unreachable manager: %v", err)
	}
	if err := answer("/etc/systemd/user\n", nil).checkServiceManager(context.Background(), &p); err == nil || !strings.Contains(err.Error(), "does not search /h/.config/systemd/user") {
		t.Errorf("manager that does not search the unit directory: %v", err)
	}
	if err := answer("/h/.config/systemd/user /etc/systemd/user\n", nil).checkServiceManager(context.Background(), &p); err != nil || p.unitFile != "/h/.config/systemd/user/wardyn-runnerd.service" {
		t.Errorf("manager that searches it: %v, unit at %q", err, p.unitFile)
	}
}

// The unit goes where the systemd user manager looks, whatever the shell's XDG_CONFIG_HOME: the manager's
// own UnitPath decides, a unit already in one of its directories is rewritten in place, and a rerun keeps
// the place (DR-0158).
func TestSetupPutsTheUnitWhereTheUserManagerLooks(t *testing.T) {
	for name, tc := range map[string]struct {
		searched []string // the manager's user configuration directories, in UnitPath order
		existing string   // where a unit is already installed, "" for none
		want     string
	}{
		"only the custom XDG directory":               {searched: []string{"xdg"}, want: "xdg"},
		"only HOME though the shell sets XDG":         {searched: []string{"home"}, want: "home"},
		"both with the unit already in the later one": {searched: []string{"xdg", "home"}, existing: "home", want: "home"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			f.host.xdgConfig = filepath.Join(filepath.Dir(f.host.home), "xdg")
			dirs := map[string]string{"home": filepath.Join(f.host.home, ".config", "systemd", "user"), "xdg": filepath.Join(f.host.xdgConfig, "systemd", "user")}
			var unitPath []string
			for _, s := range tc.searched {
				unitPath = append(unitPath, dirs[s])
			}
			unitPath = append(unitPath, "/etc/systemd/user")
			run := f.host.run
			f.host.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "systemctl" && slices.Contains(args, "UnitPath") {
					return []byte(strings.Join(unitPath, " ") + "\n"), nil
				}
				return run(ctx, name, args...)
			}
			if tc.existing != "" {
				if err := os.MkdirAll(dirs[tc.existing], 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dirs[tc.existing], serviceUnit), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, o := range []setupOptions{{tokenFile: f.tokenFile}, {}} {
				if _, err := f.run(o); err != nil {
					t.Fatal(err)
				}
			}
			if n := f.count("POST /api/v1/runners/register"); n != 1 {
				t.Errorf("registered %d times", n)
			}
			for key, dir := range dirs {
				b, err := os.ReadFile(filepath.Join(dir, serviceUnit))
				switch {
				case key == tc.want && (err != nil || !strings.Contains(string(b), "\nExecStart=")):
					t.Errorf("no rendered unit in %s: %v %q", dir, err, b)
				case key != tc.want && err == nil:
					t.Errorf("a unit was written to %s too, which the manager was not to load", dir)
				}
			}
			p, err := f.host.paths()
			if err != nil {
				t.Fatal(err)
			}
			if unit, _ := os.ReadFile(filepath.Join(dirs[tc.want], serviceUnit)); !strings.Contains(string(unit), "serve --config "+systemdQuote(p.configFile())+"\n") {
				t.Errorf("the unit does not name %s:\n%s", p.configFile(), unit)
			}
			if _, err := os.Stat(p.configFile()); err != nil {
				t.Errorf("runnerd.json is not where the unit's --config names it: %v", err)
			}
		})
	}
}

// The identity setup uses is the one the service's own record names; without a record, the one root that
// holds one ($HOME when neither does); two with no record are refused before anything is stopped, written
// or registered (DR-0163).
func TestSetupKeepsTheRecordedIdentityAndRefusesTwo(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.host.xdgConfig = filepath.Join(filepath.Dir(f.host.home), "xdg")
	homeDir, xdgDir := filepath.Join(f.host.home, ".config", "wardyn", "runner"), filepath.Join(f.host.xdgConfig, "wardyn", "runner")
	put := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stateDir := func(what, want string) {
		t.Helper()
		if p, err := f.host.paths(); err != nil || p.stateDir != want {
			t.Errorf("%s: state dir %q, %v; want %q", what, p.stateDir, err, want)
		}
	}
	stateDir("fresh, XDG set in the shell only", homeDir)
	put(filepath.Join(xdgDir, "runner.json"), "{}")
	stateDir("one identity, under XDG", xdgDir)
	put(filepath.Join(homeDir, "runner.json"), "{}")
	_, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err == nil || !strings.Contains(err.Error(), "two runner identities are on this computer, in "+homeDir+" and in "+xdgDir) {
		t.Fatalf("two identities and no record: %v", err)
	}
	if len(f.calls)+len(f.commands)+len(f.events) != 0 {
		t.Errorf("a refused setup still acted: calls %v, commands %v, steps %v", f.calls, f.commands, f.events)
	}
	envFile := filepath.Join(f.host.home, ".config", "wardyn", "runnerd.env")
	put(envFile, "WARDYN_RUNNER_ORG_URL="+f.org+"\nWARDYN_RUNNER_STATE_DIR="+xdgDir+"\n")
	stateDir("two identities, the service records XDG", xdgDir)
	put(envFile, "WARDYN_RUNNER_STATE_DIR="+homeDir+"\n")
	stateDir("two identities, the service records HOME", homeDir)
	// runnerd.json is the service's record now, and outranks one an earlier setup left.
	if err := os.Remove(envFile); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(f.host.xdgConfig, "wardyn", runnerConfigName)
	put(cfgFile, `{"state_dir": "`+xdgDir+`"}`)
	stateDir("two identities, runnerd.json records XDG", xdgDir)
	put(envFile, "WARDYN_RUNNER_STATE_DIR="+homeDir+"\n")
	stateDir("runnerd.json outranks the older env file", xdgDir)
}

// A binary path with a space, a quote or a systemd specifier stays one ExecStart argument.
func TestSystemdQuoteKeepsOnePath(t *testing.T) {
	for in, want := range map[string]string{
		"/home/a/.local/share/wardyn/bin/wardyn-runnerd": `"/home/a/.local/share/wardyn/bin/wardyn-runnerd"`,
		"/home/a b/share/wardyn-runnerd":                 `"/home/a b/share/wardyn-runnerd"`,
		`/h/100%/we"ird\$x/wardyn-runnerd`:               `"/h/100%%/we\"ird\\$$x/wardyn-runnerd"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSetupRegistersOnceAndRewritesTheServiceEachTime(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p, err := f.host.paths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Fatalf("first run registered %d times", f.count("POST /api/v1/runners/register"))
	}
	env := f.settings(p)
	for _, want := range []string{"WARDYN_RUNNER_ORG_URL=" + f.org + "\n", "WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:1\n", "WARDYN_INTERNAL_NETWORK=wardyn-internal\n", "WARDYN_RUNNER_STATE_DIR=" + p.stateDir + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("runnerd.json lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "WARDYN_RELAY_LISTEN") || strings.Contains(env, "DOCKER_HOST") {
		t.Errorf("runnerd.json sets what was not asked for:\n%s", env)
	}
	if st, err := os.Stat(p.configFile()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("runnerd.json mode: %v %v", st, err)
	}
	if unit := f.read(p.unitFile); !strings.Contains(unit, "\nExecStart=\""+p.installed+"\" serve --config ") || strings.Contains(unit, "__WARDYN_RUNNERD__ serve") {
		t.Errorf("unit not rendered:\n%s", unit)
	}
	if f.read(p.installed) != "binary" {
		t.Error("the binary was not installed")
	}
	if got := strings.Join(f.networks, ","); got != "wardyn-internal" {
		t.Errorf("network ensured as %q", got)
	}
	for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable --now wardyn-runnerd.service"} {
		if !contains(f.commands, want) {
			t.Errorf("no %q in %v", want, f.commands)
		}
	}

	// Again, as device management would: no token, the identity is there.
	before := f.read(p.configFile())
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Fatalf("an existing identity was registered again (%d)", f.count("POST /api/v1/runners/register"))
	}
	if f.read(p.configFile()) != before {
		t.Error("a second run changed runnerd.json")
	}
	if len(f.networks) != 2 {
		t.Errorf("the network was not ensured on the second run: %v", f.networks)
	}
	// A rerun handed a new token says it kept the identity instead of passing for a new registration.
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil || f.count("POST /api/v1/runners/register") != 1 || !strings.Contains(out, "already registered, so the token in "+f.tokenFile+" was not used") {
		t.Fatalf("a rerun with a token: registered %d times, %v\n%s", f.count("POST /api/v1/runners/register"), err, out)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestSetupRefusesBeforeSpendingTheToken(t *testing.T) {
	for name, tc := range map[string]struct {
		opts    setupOptions
		mutate  func(*setupFixture)
		message string
	}{
		"root":                                   {setupOptions{}, func(f *setupFixture) { f.host.uid = 0 }, "not as root"},
		"no token file":                          {setupOptions{}, nil, "no runner identity yet"},
		"missing token file":                     {setupOptions{tokenFile: "/nonexistent/token"}, nil, "token file not found"},
		"bad relay address":                      {setupOptions{relayListen: "not-an-address"}, nil, "--relay-listen"},
		"default without pool":                   {setupOptions{claim: true, makeDefault: true}, nil, "--default needs --pool"},
		"pool without claim":                     {setupOptions{pool: "Laptop"}, nil, "--pool needs --claim"},
		"unsafe setting":                         {setupOptions{network: "net work"}, nil, "unsupported characters"},
		"unsafe docker host":                     {setupOptions{}, func(f *setupFixture) { f.host.dockerHost = "unix:///run/docker.sock\nEnvironment=X=1" }, "unsupported characters"},
		"docker unusable":                        {setupOptions{}, func(f *setupFixture) { f.engineErr = os.ErrPermission }, "permission denied"},
		"relay probe unusable":                   {setupOptions{}, func(f *setupFixture) { f.relayErr = os.ErrPermission }, "No token was spent"},
		"relay address another computer reaches": {setupOptions{relayListen: "192.168.1.20"}, nil, "never an address another computer can reach"},
		"relay unreachable": {setupOptions{}, func(f *setupFixture) {
			f.relayErr = errors.New("relay: no container on Docker engine reached this runner")
		}, "No token was spent"},
		"relay host target not an address": {setupOptions{relayHostTarget: "wardynd.internal"}, nil, "--relay-host-target"},
		"relay host target loopback":       {setupOptions{relayHostTarget: "127.0.0.1"}, nil, "--relay-host-target"},
		"user manager cannot find the unit": {setupOptions{}, func(f *setupFixture) {
			f.host.run = func(context.Context, string, ...string) ([]byte, error) { return []byte("/etc/systemd/user"), nil }
		}, "no token was spent"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			if tc.mutate != nil {
				tc.mutate(f)
			}
			if tc.opts.tokenFile == "" && name != "no token file" {
				tc.opts.tokenFile = f.tokenFile
			}
			_, err := f.run(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err = %v, want %q", err, tc.message)
			}
			if len(f.calls) != 0 {
				t.Errorf("the organisation was called: %v", f.calls)
			}
			p, _ := f.host.paths()
			if _, err := os.Stat(p.installed); err == nil {
				t.Error("a refused setup installed the binary")
			}
		})
	}
}

func setupBuild(t *testing.T, v string) {
	t.Helper()
	old := buildVersion
	buildVersion = func() string { return v }
	t.Cleanup(func() { buildVersion = old })
}

// A development build has no release image. With no --proxy-image on a first registration the
// organisation may still name one, so setup refuses only after registering, and says to run it again
// without a new token; that second run is refused before anything is called, until the flag is given.
func TestSetupDevelopmentBuildNeedsAProxyImage(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.flagless = true
	setupBuild(t, "0.9.1+abc123def")

	_, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err == nil || !strings.Contains(err.Error(), "development build (0.9.1+abc123def): pass --proxy-image") || !strings.Contains(err.Error(), "no new token is needed") {
		t.Fatalf("a development build without --proxy-image and without an answer: %v", err)
	}
	if want := []string{"engine", "install", "register"}; !slices.Equal(f.events, want) {
		t.Errorf("events = %v, want %v: no probe and no service", f.events, want)
	}
	p, _ := f.host.paths()
	if _, err := os.Stat(p.configFile()); err == nil {
		t.Error("a refused setup wrote runnerd.json")
	}

	f.calls = nil
	if _, err := f.run(setupOptions{}); err == nil || !strings.Contains(err.Error(), "pass --proxy-image <ref>") || len(f.calls) != 0 {
		t.Fatalf("the re-run without the flag: err=%v calls=%v", err, f.calls)
	}
	if _, err := f.run(setupOptions{proxyImage: "local/proxy:dev"}); err != nil || f.count("POST /api/v1/runners/register") != 0 {
		t.Fatalf("the re-run with the flag: %v (registered %d more times)", err, f.count("POST /api/v1/runners/register"))
	}

	g := newSetupFixture(t, "linux")
	g.flagless = true
	setupBuild(t, "0.9.1-rc.2")
	if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
		t.Fatal(err)
	}
	gp, _ := g.host.paths()
	// The derived release image is probed but not pinned (DR-0202): serve picks its own release image.
	if env := g.settings(gp); !slices.Contains(g.checks, "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1-rc.2 wardyn-internal") || strings.Contains(env, "WARDYN_PROXY_IMAGE") {
		t.Errorf("a release candidate did not default to its own image unpinned: probed %v\n%s", g.checks, env)
	}
}

// Setup probes before the token is spent with the best image it knows (--proxy-image, the saved
// identity's, else the release image). After registering it probes again only if the organisation's
// answer names a different image, and the env file carries the image that was last probed.
func TestSetupProbesWhenItKnowsTheProxyImage(t *testing.T) {
	setupBuild(t, "0.9.1")
	const release, mirror = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1", "mirror.example.com/wardyn-proxy:0.9.1"
	f := newSetupFixture(t, "linux")
	f.flagless, f.answered = true, mirror
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"engine", "probe", "install", "register", "probe", "service"}; !slices.Equal(f.events, want) {
		t.Errorf("a first flagless registration did %v, want %v", f.events, want)
	}
	if want := []string{release + " wardyn-internal", mirror + " wardyn-internal"}; !slices.Equal(f.checks, want) {
		t.Errorf("probed %q, want the release image before the token and the organisation's after", f.checks)
	}
	// The organisation's answer is the identity's, and the file does not pin it: serve keeps following the organisation.
	if env, id := f.settings(mustPaths(t, f)), f.read(mustPaths(t, f).identity()); strings.Contains(env, "WARDYN_PROXY_IMAGE") || !strings.Contains(id, mirror) {
		t.Errorf("the organisation's image is pinned in runnerd.json or not saved in the identity:\n%s\n%s", env, id)
	}

	// Registered with that image saved: one probe, first, with the saved image, and no register.
	f.events, f.checks = nil, nil
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"engine", "probe", "install", "service"}; !slices.Equal(f.events, want) {
		t.Errorf("a re-run did %v, want %v", f.events, want)
	}
	if want := []string{mirror + " wardyn-internal"}; !slices.Equal(f.checks, want) {
		t.Errorf("the re-run probed with %q", f.checks)
	}

	// An answer that matches the probed image, or none, is not probed again.
	for _, answered := range []string{"", release} {
		g := newSetupFixture(t, "linux")
		g.flagless, g.answered = true, answered
		if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"engine", "probe", "install", "register", "service"}; !slices.Equal(g.events, want) {
			t.Errorf("answer %q: did %v, want %v", answered, g.events, want)
		}
	}

	// A flag probes before register, once, and wins over the answer.
	h := newSetupFixture(t, "linux")
	h.answered = mirror
	if _, err := h.run(setupOptions{tokenFile: h.tokenFile}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"engine", "probe", "install", "register", "service"}; !slices.Equal(h.events, want) {
		t.Errorf("a flagged registration did %v, want %v", h.events, want)
	}
	if env := h.settings(mustPaths(t, h)); !strings.Contains(env, "WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:1\n") {
		t.Errorf("the flag did not win over the answer:\n%s", env)
	}
}

func mustPaths(t *testing.T, f *setupFixture) setupPaths {
	t.Helper()
	p, err := f.host.paths()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The release image is only a default: when this engine cannot get it (an air-gapped estate) setup does
// not refuse before the token but probes the organisation's answer after registering. An image the person
// named (a flag, or the saved identity's) that cannot be got, and a probe that ran and failed, still refuse
// before the token.
func TestSetupGivesUpOnAnUnreachableReleaseImageOnly(t *testing.T) {
	setupBuild(t, "0.9.1")
	const release, mirror = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1", "mirror.example.com/wardyn-proxy:0.9.1"
	f := newSetupFixture(t, "linux")
	f.flagless, f.unpullable, f.answered = true, release, mirror
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"engine", "probe", "install", "register", "probe", "service"}; !slices.Equal(f.events, want) {
		t.Errorf("events = %v, want %v", f.events, want)
	}
	if want := []string{release + " wardyn-internal", mirror + " wardyn-internal"}; !slices.Equal(f.checks, want) {
		t.Errorf("probed %q", f.checks)
	}
	if !strings.Contains(out, "The release proxy image "+release+" is not reachable from this engine; the organisation's answer will be used.") {
		t.Errorf("the give-up was not said:\n%s", out)
	}
	// The organisation's answer is the identity's, and the file does not pin it: serve keeps following the organisation.
	if env, id := f.settings(mustPaths(t, f)), f.read(mustPaths(t, f).identity()); strings.Contains(env, "WARDYN_PROXY_IMAGE") || !strings.Contains(id, mirror) {
		t.Errorf("the organisation's image is pinned in runnerd.json or not saved in the identity:\n%s\n%s", env, id)
	}

	// Nothing named by the organisation either: refused after registering, with no new token needed.
	g := newSetupFixture(t, "linux")
	g.flagless, g.unpullable = true, release
	if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err == nil || !strings.Contains(err.Error(), "not reachable from this engine") || !strings.Contains(err.Error(), "no new token is needed") {
		t.Fatalf("no answer and no release image: %v", err)
	}
	if contains(g.commands, "systemctl --user enable --now wardyn-runnerd.service") {
		t.Error("the service was started")
	}

	// A probe that ran and failed refuses before the token, whatever the image's source.
	h := newSetupFixture(t, "linux")
	h.flagless, h.relayErr, h.answered = true, errors.New("relay: no container reached this runner"), mirror
	if _, err := h.run(setupOptions{tokenFile: h.tokenFile}); err == nil || !strings.Contains(err.Error(), "No token was spent") || len(h.calls) != 0 {
		t.Fatalf("a failed probe of the release image: err=%v calls=%v", err, h.calls)
	}

	// A flag the person named, and a saved identity's image, refuse before the token when unpullable.
	i := newSetupFixture(t, "linux")
	i.unpullable = "ghcr.io/example/proxy:1"
	if _, err := i.run(setupOptions{tokenFile: i.tokenFile}); err == nil || !strings.Contains(err.Error(), "No token was spent") || len(i.calls) != 0 {
		t.Fatalf("an unpullable flag image: err=%v calls=%v", err, i.calls)
	}
	j := newSetupFixture(t, "linux")
	j.flagless, j.answered = true, mirror
	if _, err := j.run(setupOptions{tokenFile: j.tokenFile}); err != nil {
		t.Fatal(err)
	}
	j.calls, j.unpullable = nil, mirror
	if _, err := j.run(setupOptions{}); err == nil || !strings.Contains(err.Error(), "saved proxy image") || len(j.calls) != 0 {
		t.Fatalf("an unpullable saved image: err=%v calls=%v", err, j.calls)
	}
	// A registered computer whose identity names nothing has only the release image to fall back on.
	k := newSetupFixture(t, "linux")
	k.flagless = true
	if _, err := k.run(setupOptions{tokenFile: k.tokenFile}); err != nil {
		t.Fatal(err)
	}
	k.calls, k.unpullable = nil, release
	if _, err := k.run(setupOptions{}); err == nil || !strings.Contains(err.Error(), "No token was spent") {
		t.Fatalf("a registered computer with an unpullable release image: %v", err)
	}
}

// With no image known before registering (a development build, no flag) the first probe runs after the
// answer, still before the service is installed. A failure starts no service and says no new token is
// needed, and the re-run, which takes the image from the saved identity, succeeds without registering again.
func TestSetupDeferredProbeFailureNeedsNoNewToken(t *testing.T) {
	setupBuild(t, "0.9.1+abc123def")
	f := newSetupFixture(t, "linux")
	f.flagless, f.answered = true, "mirror.example.com/wardyn-proxy:0.9.1"
	f.relayErr = errors.New("relay: no container reached this runner")
	_, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err == nil || !strings.Contains(err.Error(), "no container reached") || !strings.Contains(err.Error(), "no new token is needed") {
		t.Fatalf("deferred probe failure: %v", err)
	}
	if want := []string{"engine", "install", "register", "probe"}; !slices.Equal(f.events, want) {
		t.Errorf("events = %v, want %v", f.events, want)
	}
	if contains(f.commands, "systemctl --user enable --now wardyn-runnerd.service") {
		t.Error("a failed probe started the service")
	}
	if _, serr := os.Stat(mustPaths(t, f).unitFile); serr == nil {
		t.Error("a failed probe installed the unit")
	}
	f.relayErr = nil
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatalf("re-run without a token: %v", err)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Errorf("the re-run registered again (%d)", f.count("POST /api/v1/runners/register"))
	}
}

// An image the organisation names that cannot go into a unit file is never written anywhere. When the
// image probed before the token works, setup keeps it and warns; with none, it refuses after registering.
func TestSetupRefusesAnUnsafeAnsweredImage(t *testing.T) {
	const release = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1"
	setupBuild(t, "0.9.1")
	f := newSetupFixture(t, "linux")
	f.flagless, f.answered = true, "evil\nExecStart=/bin/sh"
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil || !strings.Contains(out, "keeping "+release) {
		t.Fatalf("unsafe answered image with a probed release image: err=%v\n%s", err, out)
	}
	// The probed image is this binary's release image, which serve picks unpinned (DR-0202).
	if env := f.settings(mustPaths(t, f)); strings.Contains(env, "WARDYN_PROXY_IMAGE") || strings.Contains(env, "ExecStart") {
		t.Errorf("the env file pins the derived release image or the answer:\n%s", env)
	}

	g := newSetupFixture(t, "linux")
	g.flagless, g.answered, g.unpullable = true, "evil\nExecStart=/bin/sh", release
	if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err == nil || !strings.Contains(err.Error(), "unsupported characters") || !strings.Contains(err.Error(), "no new token is needed") {
		t.Fatalf("unsafe answered image with nothing probed: %v", err)
	}
	if _, serr := os.Stat(mustPaths(t, g).configFile()); serr == nil {
		t.Error("an unsafe image reached runnerd.json")
	}
}

// A probe that fails with the image the organisation named, or with the image saved in runner.json, says
// to run setup again with --proxy-image for this computer's architecture, and that no new token is needed.
func TestSetupNamesTheArchitectureWhenTheNamedImageFails(t *testing.T) {
	const mirror = "mirror.example.com/wardyn-proxy:0.9.1"
	setupBuild(t, "0.9.1")
	hint := "run setup again with --proxy-image <ref> for this computer's architecture (arm64) (no new token is needed)"

	f := newSetupFixture(t, "linux")
	f.flagless, f.answered, f.unpullable = true, mirror, mirror
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err == nil || !strings.Contains(err.Error(), hint) {
		t.Fatalf("answer image fails after registering: %v", err)
	}

	g := newSetupFixture(t, "linux")
	g.flagless, g.answered = true, mirror
	if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
		t.Fatal(err)
	}
	g.unpullable = mirror
	if _, err := g.run(setupOptions{}); err == nil || !strings.Contains(err.Error(), hint) || !strings.Contains(err.Error(), "runner.json") {
		t.Fatalf("saved image fails on a re-run: %v", err)
	}
	// A flag the person typed gets no such hint: they named it.
	if _, err := g.run(setupOptions{proxyImage: mirror}); err == nil || strings.Contains(err.Error(), "architecture") {
		t.Fatalf("flag image fails on a re-run: %v", err)
	}
}

// On a VM-backed engine setup pins the pair its check proved, in the env file and the LaunchAgent, and
// says which; on a native engine it writes neither, so serve reads the bridge gateway at every start.
func TestSetupWritesTheCheckedRelayPair(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		f.vm = true
		out, err := f.run(setupOptions{tokenFile: f.tokenFile})
		if err != nil {
			t.Fatal(err)
		}
		p, _ := f.host.paths()
		got := f.settings(p)
		for _, want := range []string{"WARDYN_RELAY_LISTEN=127.0.0.1\n", "WARDYN_RELAY_HOST_TARGET=192.168.5.2\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: runnerd.json lacks %q:\n%s", goos, want, got)
			}
		}
		if line := `Docker engine "Alpine Linux v3.21" (lima-rancher-desktop): the run relay listens on 127.0.0.1 and a run's proxy reaches it at 192.168.5.2`; !strings.Contains(out, line) {
			t.Errorf("%s: output does not name the engine and pair:\n%s", goos, out)
		}

		d := newSetupFixture(t, goos)
		d.desktop = true
		if _, err := d.run(setupOptions{tokenFile: d.tokenFile}); err != nil {
			t.Fatal(err)
		}
		dp, _ := d.host.paths()
		if got := d.settings(dp); !strings.Contains(got, "WARDYN_RELAY_HOST_TARGET=host-gateway\n") {
			t.Errorf("%s: a VM-backed engine's host-gateway target was not pinned:\n%s", goos, got)
		}

		n := newSetupFixture(t, goos)
		if _, err := n.run(setupOptions{tokenFile: n.tokenFile}); err != nil {
			t.Fatal(err)
		}
		np, _ := n.host.paths()
		got = n.settings(np)
		if strings.Contains(got, "WARDYN_RELAY_") {
			t.Errorf("%s: a native engine's relay was pinned:\n%s", goos, got)
		}
	}
}

// The explicit relay values reach the engine check unchanged, and setup checks with the image it installs.
func TestSetupHandsExplicitRelayFlagsToTheCheck(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.vm = true
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, relayListen: "127.0.0.1", relayHostTarget: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if want := []relayPair{pair("127.0.0.1", "192.0.2.1")}; !slices.Equal(f.wants, want) {
		t.Errorf("check was handed %v, want %v", f.wants, want)
	}
	if want := []string{"ghcr.io/example/proxy:1 wardyn-internal"}; !slices.Equal(f.checks, want) {
		t.Errorf("checks = %q, want %q", f.checks, want)
	}
}

// An explicit relay host target is written on a native engine too: setup never drops a flag.
func TestSetupWritesTheRelayHostTarget(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, relayHostTarget: "10.0.0.9"}); err != nil {
			t.Fatal(err)
		}
		p, _ := f.host.paths()
		if got := f.settings(p); !strings.Contains(got, "WARDYN_RELAY_HOST_TARGET=10.0.0.9\n") || strings.Contains(got, "WARDYN_RELAY_LISTEN") {
			t.Errorf("%s: runnerd.json lacks the target or pins a native relay address:\n%s", goos, got)
		}
	}
}

func TestSetupAcceptsTheEnginesHostGatewayAsRelayAddress(t *testing.T) {
	f := newSetupFixture(t, "linux")
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, relayListen: "172.17.0.1"}); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRendersTheLaunchAgent(t *testing.T) {
	f := newSetupFixture(t, "darwin")
	f.desktop = true
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.host.paths()
	plist := f.read(p.plistFile)
	for _, want := range []string{"<string>" + p.installed + "</string>", "<string>--config</string>\n    <string>" + p.configFile() + "</string>", "<string>" + p.logFile + "</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "__") || strings.Contains(plist, "EnvironmentVariables") || strings.Contains(plist, "<!--") {
		t.Errorf("plist keeps a placeholder, an environment or the template comment:\n%s", plist)
	}
	if got := f.settings(p); !strings.Contains(got, "WARDYN_RELAY_LISTEN=127.0.0.1\n") || !strings.Contains(got, "WARDYN_RUNNER_ORG_URL="+f.org+"\n") {
		t.Errorf("runnerd.json lacks the relay address or the organisation:\n%s", got)
	}
	if !contains(f.commands, "launchctl bootstrap gui/501 "+p.plistFile) {
		t.Errorf("agent not bootstrapped: %v", f.commands)
	}
	// The package's agent must be stopped before the setup agent starts, or two serves share one identity.
	if i, j := slices.Index(f.commands, "launchctl bootout gui/501/com.wardyn.runnerd.managed"), slices.Index(f.commands, "launchctl bootstrap gui/501 "+p.plistFile); i < 0 || j < 0 || i > j {
		t.Errorf("the managed agent was not booted out before bootstrap: %v", f.commands)
	}
	if !strings.Contains(out, `Docker engine "Docker Desktop" (docker-desktop): the run relay listens on 127.0.0.1 and a run's proxy reaches it at host-gateway`) {
		t.Errorf("the engine and relay pair were not announced:\n%s", out)
	}
	if st, err := os.Stat(p.installed); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("installed binary mode: %v %v", st, err)
	}
}

func TestSetupWithoutClaimLeavesTheClaimToThePerson(t *testing.T) {
	f := newSetupFixture(t, "linux")
	t.Setenv("WARDYN_TOKEN", "wdn_secret")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/claim") || strings.Contains(c, "/me/") {
			t.Fatalf("setup without --claim called %s", c)
		}
	}
	if !strings.Contains(out, "Key fingerprint ") || !strings.Contains(out, "My runner") {
		t.Errorf("no fingerprint and console path:\n%s", out)
	}
}

func TestSetupClaimWithoutPersonalTokenPrintsTheFingerprint(t *testing.T) {
	f := newSetupFixture(t, "linux")
	t.Setenv("WARDYN_TOKEN", "")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, claim: true, pool: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/me/runners/"+f.id.String()+"/claim") != 0 {
		t.Fatal("claimed without a personal token")
	}
	if !strings.Contains(out, "--claim needs WARDYN_TOKEN") || !strings.Contains(out, "Key fingerprint ") {
		t.Errorf("no explanation:\n%s", out)
	}
}

func TestSetupClaimsIntoAPoolOnceAndOnlyOnce(t *testing.T) {
	f := newSetupFixture(t, "linux")
	t.Setenv("WARDYN_TOKEN", "wdn_secret")
	opts := setupOptions{tokenFile: f.tokenFile, claim: true, pool: "Laptop", makeDefault: true}
	out, err := f.run(opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	claim := "POST /api/v1/me/runners/" + f.id.String() + "/claim"
	if f.count(claim) != 1 || f.state != types.RunnerClaimed || len(f.pools) != 1 || f.pools[0].Name != "Laptop" || f.defaultID == nil || *f.defaultID != f.pools[0].ID {
		t.Fatalf("claim=%d state=%s pools=%v default=%v", f.count(claim), f.state, f.pools, f.defaultID)
	}
	if !strings.Contains(out, "default pool set to Laptop") {
		t.Errorf("output:\n%s", out)
	}

	// A repeat run: registered, claimed and in the pool already, so nothing is made or claimed again.
	out, err = f.run(opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.count(claim) != 1 || len(f.pools) != 1 || f.count("POST /api/v1/runners/register") != 1 || !strings.Contains(out, "already claimed") {
		t.Fatalf("repeat: claim=%d pools=%d registers=%d\n%s", f.count(claim), len(f.pools), f.count("POST /api/v1/runners/register"), out)
	}
}

func TestSetupSaysWhenTheClaimFailedAfterTheServiceStarted(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.refuseClaim = true
	t.Setenv("WARDYN_TOKEN", "wdn_secret")
	_, err := f.run(setupOptions{tokenFile: f.tokenFile, claim: true})
	if err == nil || !strings.Contains(err.Error(), "installed and running but not claimed") || !strings.Contains(err.Error(), "wardyn runner claim") {
		t.Fatalf("err = %v", err)
	}
	p, _ := f.host.paths()
	if _, serr := os.Stat(p.unitFile); serr != nil {
		t.Error("the service was not installed before the claim")
	}
}

// An administrator's shared pool list also holds other people's user pools; setup reads /me/runner-pools,
// which does not, so a same-named pool that is not the caller's is neither picked nor reported as a match.
func TestSetupPoolIgnoresAnotherPersonsSameNamedPool(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.otherPool = &sdk.RunnerPool{ID: uuid.New(), Name: "Laptop", Scope: sdk.RunnerPoolUser}
	t.Setenv("WARDYN_TOKEN", "wdn_secret")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, claim: true, pool: "Laptop", makeDefault: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.pools) != 1 || f.defaultID == nil || *f.defaultID != f.pools[0].ID || *f.defaultID == f.otherPool.ID {
		t.Fatalf("pools=%v default=%v other=%s", f.pools, f.defaultID, f.otherPool.ID)
	}
	if f.count("GET /api/v1/runner-pools") != 0 || f.count("GET /api/v1/me/runner-pools") == 0 {
		t.Error("setup did not read the pool lookup from /me/runner-pools alone")
	}
	if f.count("PUT /api/v1/me/runner-pools/"+f.otherPool.ID.String()+"/runners/"+f.id.String()) != 0 {
		t.Error("the runner was put in another person's pool")
	}
}

// The proxy image setup settled reaches registration, so a VM-backed engine's disk is probed with it.
func TestSetupProbesTheVMDiskWithTheNamedProxyImage(t *testing.T) {
	f := newSetupFixture(t, "linux")
	asked := stubDisk(t, types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384}, 204800, true)
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: "local/proxy:dev"}); err != nil || *asked != "local/proxy:dev" {
		t.Fatalf("the VM disk was probed with %q, %v; want local/proxy:dev", *asked, err)
	}
}

// Setup says once that runs have no disk cap, and nothing when the engine caps them.
func TestSetupSaysWhenTheEngineHasNoDiskCap(t *testing.T) {
	old := engineDiskCapped
	t.Cleanup(func() { engineDiskCapped = old })
	for _, tc := range []struct {
		capped, known bool
		want          int
	}{{false, true, 1}, {true, true, 0}, {false, false, 0}} {
		engineDiskCapped = func(context.Context) (bool, bool) { return tc.capped, tc.known }
		f := newSetupFixture(t, "linux")
		out, err := f.run(setupOptions{tokenFile: f.tokenFile})
		if n := strings.Count(out, noDiskCapLine); err != nil || n != tc.want {
			t.Errorf("capped=%v known=%v: the no-cap line appeared %d times, want %d (%v)\n%s", tc.capped, tc.known, n, tc.want, err, out)
		}
	}
}

// --no-service registers and installs the binary, writes the environment file and prints the command
// that runs the runner, and installs nothing a login would start: no unit, no plist, no service
// command, and no check that a systemd user manager would find a unit.
func TestSetupNoServiceWritesTheEnvironmentAndInstallsNoUnit(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			f.host.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				f.commands = append(f.commands, name+" "+strings.Join(args, " "))
				return []byte("/nowhere"), nil // a user manager that would never find the unit
			}
			p, _ := f.host.paths()
			out, err := f.run(setupOptions{tokenFile: f.tokenFile, noService: true})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if f.count("POST /api/v1/runners/register") != 1 || f.read(p.installed) != "binary" {
				t.Fatal("--no-service did not register or install the binary")
			}
			env := f.settings(p)
			for _, want := range []string{"WARDYN_RUNNER_ORG_URL=" + f.org + "\n", "WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:1\n", "WARDYN_PROXY_IMAGE_PINNED=1\n", "WARDYN_RUNNER_STATE_DIR=" + p.stateDir + "\n"} {
				if !strings.Contains(env, want) {
					t.Errorf("runnerd.json lacks %q:\n%s", want, env)
				}
			}
			if st, err := os.Stat(p.configFile()); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("runnerd.json mode: %v %v", st, err)
			}
			for _, path := range []string{p.unitFile, p.plistFile} {
				if path == "" {
					continue
				}
				if _, err := os.Stat(path); err == nil {
					t.Errorf("--no-service installed %s", path)
				}
			}
			if len(f.commands) != 0 {
				t.Errorf("--no-service ran %v", f.commands)
			}
			if want := shellQuote(p.installed) + " serve --config " + shellQuote(p.configFile()); !strings.Contains(out, want) {
				t.Errorf("the run command is not printed (want %q):\n%s", want, out)
			}
			if strings.Contains(out, "Started the wardyn-runnerd") {
				t.Errorf("--no-service claims to have started a service:\n%s", out)
			}

			// Setup run again without --proxy-image keeps what it wrote.
			f.flagless = true
			if _, err := f.run(setupOptions{noService: true}); err != nil {
				t.Fatal(err)
			}
			if f.settings(p) != env {
				t.Error("a second --no-service run changed runnerd.json")
			}
		})
	}
}

// Setup names the engine it checked in the service every time, the default socket included, so the
// service never consults the person's docker context and cannot be moved by a later `docker context use`.
func TestSetupPinsTheEngineInTheService(t *testing.T) {
	for _, tc := range []struct{ env, want string }{{"", defaultDockerSocket}, {"unix:///home/me/.colima/default/docker.sock", "unix:///home/me/.colima/default/docker.sock"}} {
		t.Setenv("DOCKER_HOST", tc.env)
		host, err := osSetupHost()
		if err != nil {
			t.Fatal(err)
		}
		if host.dockerHost != tc.want {
			t.Fatalf("DOCKER_HOST=%q: setup host engine = %q, want %q", tc.env, host.dockerHost, tc.want)
		}
		if got := (setupPlan{dockerHost: host.dockerHost}).runnerFile().DockerHost; got != tc.want {
			t.Errorf("runnerd.json docker_host = %q, want %q", got, tc.want)
		}
	}
}

// On macOS every path holds "Application Support". The command --no-service prints, run through sh, must
// reach the binary with the config path as one argument.
func TestSetupNoServicePrintsACommandAShellRunsWithASpaceInThePath(t *testing.T) {
	f := newSetupFixture(t, "darwin")
	stub := "#!/bin/sh\nfor a; do printf 'arg=%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(f.host.exe, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := f.host.paths()
	if !strings.Contains(p.stateDir, " ") {
		t.Fatalf("the macOS state directory %q has no space; the test proves nothing", p.stateDir)
	}
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, noService: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var command string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, " serve --config ") {
			command = strings.TrimSpace(l)
		}
	}
	if command == "" {
		t.Fatalf("no run command printed:\n%s", out)
	}
	got, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c %q: %v\n%s", command, err, got)
	}
	for _, want := range []string{"arg=serve\n", "arg=--config\narg=" + p.configFile() + "\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the printed command's run lacks %q:\n%s", want, got)
		}
	}
}

// Setup run plain after a --no-service run and then a service setup that pinned another image and CA file
// keeps the service's pins, not the earlier run's.
func TestSetupKeepsTheLatestPinsAfterANoServiceRun(t *testing.T) {
	f := newSetupFixture(t, "darwin")
	dir := t.TempDir()
	caX := writeFile(t, filepath.Join(dir, "x.pem"), testCAPEM(t, "x"))
	caZ := writeFile(t, filepath.Join(dir, "z.pem"), testCAPEM(t, "z"))
	p, _ := f.host.paths()
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, noService: true, proxyImage: "ghcr.io/example/proxy:x", trustedCAFile: caX}); err != nil {
		t.Fatal(err)
	}
	if env := f.settings(p); !strings.Contains(env, "proxy:x") {
		t.Fatalf("the --no-service run wrote no X pin:\n%s", env)
	}
	if _, err := f.run(setupOptions{proxyImage: "ghcr.io/example/proxy:z", trustedCAFile: caZ}); err != nil {
		t.Fatal(err)
	}
	f.flagless = true
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	env := f.settings(p)
	for _, want := range []string{"proxy:z", caZ, "WARDYN_PROXY_IMAGE_PINNED=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("the plain setup dropped %q from runnerd.json:\n%s", want, env)
		}
	}
	if strings.Contains(env, "proxy:x") || strings.Contains(env, caX) {
		t.Errorf("the plain setup took the earlier run's pins:\n%s", env)
	}
}

// A macOS service setup removes the environment file a --no-service run before runnerd.json wrote, so
// deleting the plist later and running --no-service again does not read the old pins back.
func TestSetupLaunchAgentRemovesTheStaleNoServiceEnvironment(t *testing.T) {
	f := newSetupFixture(t, "darwin")
	p, _ := f.host.paths()
	if err := os.MkdirAll(filepath.Dir(p.noServiceEnvFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.noServiceEnvFile(), []byte("WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:x\nWARDYN_PROXY_IMAGE_PINNED=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: "ghcr.io/example/proxy:z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.noServiceEnvFile()); !os.IsNotExist(err) {
		t.Fatalf("the LaunchAgent setup left the old environment file: %v", err)
	}
	if err := os.Remove(p.plistFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p.configFile()); err != nil {
		t.Fatal(err)
	}
	f.flagless = true
	if _, err := f.run(setupOptions{noService: true}); err != nil {
		t.Fatal(err)
	}
	if env := f.settings(p); strings.Contains(env, "proxy:x") || strings.Contains(env, "proxy:z") {
		t.Errorf("--no-service after the plist went read old pins back:\n%s", env)
	}
}

// A path that holds a line break cannot be written to the environment file --no-service reads back, so
// setup refuses it on every platform.
func TestSetupRefusesATrustedCAPathWithALineBreak(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			ca := writeFile(t, filepath.Join(t.TempDir(), "ca\n.pem"), testCAPEM(t, "x"))
			if _, err := f.run(setupOptions{tokenFile: f.tokenFile, noService: true, trustedCAFile: ca}); err == nil || !strings.Contains(err.Error(), "unsupported characters") {
				t.Fatalf("a CA path with a line break: %v", err)
			}
		})
	}
}

// --no-service is refused, before a token is spent, while a service for this runner is installed: running
// both would be two processes with one identity.
func TestSetupNoServiceRefusesWhileAServiceIsInstalled(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
				t.Fatal(err)
			}
			registered := f.count("POST /api/v1/runners/register")
			_, err := f.run(setupOptions{noService: true})
			if err == nil || !strings.Contains(err.Error(), "already installed") {
				t.Fatalf("--no-service beside an installed service = %v, want a refusal", err)
			}
			if f.count("POST /api/v1/runners/register") != registered {
				t.Error("the refusal came after the registration")
			}
		})
	}
}

// The service setup installs does not get the shell's proxy variables, so setup says so, by name and
// never by value: a proxy URL may hold a credential.
func TestSetupWarnsServiceMissesShellProxy(t *testing.T) {
	const secret = "http://u:secret@p:3128"
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			t.Setenv("HTTPS_PROXY", secret)
			t.Setenv("all_proxy", secret)
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "HTTPS_PROXY, all_proxy are set in this shell, but the service setup installs does not receive it") ||
				!strings.Contains(out, "docs/runner-registration.md#a-runner-behind-a-corporate-proxy") {
				t.Errorf("no proxy warning:\n%s", out)
			}
			if strings.Contains(out, "secret") || strings.Contains(out, "p:3128") {
				t.Errorf("the warning prints a proxy value:\n%s", out)
			}
		})
	}
	t.Run("no proxy variable", func(t *testing.T) {
		f := newSetupFixture(t, "linux")
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
			t.Setenv(k, "")
		}
		if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil || strings.Contains(out, "does not receive it") {
			t.Fatalf("a shell without a proxy was warned: %v\n%s", err, out)
		}
	})
}

// The service-proxy warning names the variables only: a value with a user name and password, and the
// proxy's host and port, appear nowhere in setup's output or log.
func TestSetupProxyWarningPrintsNamesOnly(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := newSetupFixture(t, "linux")
	for _, k := range []string{"https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTPS_PROXY", "http://u:p@proxy:3128")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HTTPS_PROXY is set in this shell, but the service setup installs does not receive it") {
		t.Errorf("no warning naming HTTPS_PROXY:\n%s", out)
	}
	for _, leak := range []string{"u:p", "proxy:3128"} {
		if strings.Contains(out, leak) || strings.Contains(logs.String(), leak) {
			t.Errorf("setup printed or logged %q:\nout:\n%s\nlog:\n%s", leak, out, logs.String())
		}
	}
}

// settings is what the service runs with, as the KEY=value lines a service environment held before
// runnerd.json: the file's fields under their old variable names, so assertions about a setting read the same.
func (f *setupFixture) settings(p setupPaths) string {
	f.t.Helper()
	file, err := loadRunnerFile(p.configFile(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	var out strings.Builder
	for _, kv := range [][2]string{{"WARDYN_RUNNER_ORG_URL", file.OrgURL}, {"WARDYN_RUNNER_STATE_DIR", file.StateDir}, {"WARDYN_INTERNAL_NETWORK", file.InternalNetwork},
		{"DOCKER_HOST", file.DockerHost}, {"WARDYN_PROXY_IMAGE", file.ProxyImage}, {"WARDYN_RELAY_LISTEN", file.RelayListen},
		{"WARDYN_RELAY_HOST_TARGET", file.RelayHostTarget}, {"WARDYN_TRUSTED_CA_FILE", file.TrustedCAFile}} {
		if kv[1] != "" {
			out.WriteString(kv[0] + "=" + kv[1] + "\n")
		}
	}
	if file.ProxyImage != "" {
		out.WriteString("WARDYN_PROXY_IMAGE_PINNED=1\n")
	}
	return out.String()
}
