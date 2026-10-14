// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
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
			if name == "systemctl" && len(args) > 2 && args[1] == "show" {
				return []byte(filepath.Join(f.host.home, ".config", "systemd", "user") + " /etc/systemd/user\n"), nil
			}
			return nil, nil
		},
		docker: func(_ context.Context, network string) (engineFacts, error) {
			f.networks = append(f.networks, network)
			return f.engine(), nil
		}}
	if err := os.MkdirAll(f.host.home, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// engine answers as a native engine whose bridge holds its host-gateway, as Docker Desktop, or as a
// VM-backed engine whose bridge is not on this host.
func (f *setupFixture) engine() engineFacts {
	gw := netip.MustParseAddr("172.17.0.1")
	switch {
	case f.desktop:
		return engineFacts{os: "Docker Desktop", name: "docker-desktop", desktop: true}
	case f.vm:
		return engineFacts{os: "Alpine Linux v3.21", name: "lima-rancher-desktop", gateway: gw}
	}
	return engineFacts{os: "Ubuntu 24.04 LTS", name: "laptop", gateway: gw, hostBridge: true, bridge: []netip.Addr{gw}}
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
		w.WriteHeader(http.StatusCreated)
		reply(types.RunnerRegistration{RunnerID: f.id, Fingerprint: runnerwire.Fingerprint(req.PublicKey), State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(f.org)})
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
	if o.proxyImage == "" {
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
	env := f.read(p.envFile)
	for _, want := range []string{"WARDYN_RUNNER_ORG_URL=" + f.org + "\n", "WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:1\n", "WARDYN_INTERNAL_NETWORK=wardyn-internal\n", "WARDYN_RUNNER_STATE_DIR=" + p.stateDir + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env file lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "WARDYN_RELAY_LISTEN") || strings.Contains(env, "DOCKER_HOST") {
		t.Errorf("env file sets what was not asked for:\n%s", env)
	}
	if st, err := os.Stat(p.envFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("env file mode: %v %v", st, err)
	}
	if unit := f.read(p.unitFile); !strings.Contains(unit, "\nExecStart="+p.installed+" serve --org ") || strings.Contains(unit, "__WARDYN_RUNNERD__ serve") {
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
	before := f.read(p.envFile)
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Fatalf("an existing identity was registered again (%d)", f.count("POST /api/v1/runners/register"))
	}
	if f.read(p.envFile) != before {
		t.Error("a second run changed the env file")
	}
	if len(f.networks) != 2 {
		t.Errorf("the network was not ensured on the second run: %v", f.networks)
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
		"root":                 {setupOptions{}, func(f *setupFixture) { f.host.uid = 0 }, "not as root"},
		"no token file":        {setupOptions{}, nil, "no runner identity yet"},
		"missing token file":   {setupOptions{tokenFile: "/nonexistent/token"}, nil, "token file not found"},
		"bad relay address":    {setupOptions{relayListen: "not-an-address"}, nil, "--relay-listen"},
		"default without pool": {setupOptions{claim: true, makeDefault: true}, nil, "--default needs --pool"},
		"pool without claim":   {setupOptions{pool: "Laptop"}, nil, "--pool needs --claim"},
		"unsafe setting":       {setupOptions{network: "net work"}, nil, "unsupported characters"},
		"docker unusable": {setupOptions{}, func(f *setupFixture) {
			f.host.docker = func(context.Context, string) (engineFacts, error) { return engineFacts{}, os.ErrPermission }
		}, "permission denied"},
		"relay address the engine refuses":    {setupOptions{relayListen: "127.0.0.1"}, nil, "reaches only its host-gateway 172.17.0.1"},
		"VM-backed engine":                    {setupOptions{}, func(f *setupFixture) { f.vm = true }, `Name "lima-rancher-desktop"`},
		"VM-backed engine and explicit relay": {setupOptions{relayListen: "127.0.0.1"}, func(f *setupFixture) { f.vm = true }, `Name "lima-rancher-desktop"`},
		"bad relay host target":               {setupOptions{relayHostTarget: "127.0.0.1"}, nil, "--relay-host-target"},
		"relay unreachable": {setupOptions{}, func(f *setupFixture) {
			old := probeRelay
			probeRelay = func(context.Context, netip.Addr, string) error { return errors.New("relay probe: unreachable") }
			f.t.Cleanup(func() { probeRelay = old })
		}, "relay probe: unreachable"},
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

func TestSetupDevelopmentBuildNeedsAProxyImage(t *testing.T) {
	f := newSetupFixture(t, "linux")
	build := func(v string) {
		old := buildVersion
		buildVersion = func() string { return v }
		t.Cleanup(func() { buildVersion = old })
	}
	cmd := newSetupCommand()
	cmd.SetOut(&strings.Builder{})
	cmd.SetContext(context.Background())

	build("0.9.1+abc123def")
	err := runSetup(cmd, f.host, setupOptions{org: f.org, tokenFile: f.tokenFile, network: "wardyn-internal"})
	if err == nil || !strings.Contains(err.Error(), "development build (0.9.1+abc123def): pass --proxy-image") || len(f.calls) != 0 {
		t.Fatalf("a development build without --proxy-image: err=%v calls=%v", err, f.calls)
	}

	build("0.9.1-rc.2")
	if err := runSetup(cmd, f.host, setupOptions{org: f.org, tokenFile: f.tokenFile, network: "wardyn-internal"}); err != nil {
		t.Fatal(err)
	}
	p, _ := f.host.paths()
	if env := f.read(p.envFile); !strings.Contains(env, "WARDYN_PROXY_IMAGE=ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1-rc.2\n") {
		t.Errorf("a release candidate did not default to its own image:\n%s", env)
	}
}

func TestSetupDockerDesktopGetsLoopbackRelay(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.desktop = true
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatal(err)
	}
	p, _ := f.host.paths()
	if env := f.read(p.envFile); !strings.Contains(env, "WARDYN_RELAY_LISTEN=127.0.0.1\n") {
		t.Errorf("Docker Desktop did not get a loopback relay:\n%s", env)
	}
}

// The relay host target reaches the env file and the LaunchAgent; the default, host-gateway, reaches neither.
func TestSetupWritesTheRelayHostTarget(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, relayHostTarget: "10.0.0.9"}); err != nil {
			t.Fatal(err)
		}
		p, _ := f.host.paths()
		file, want := p.envFile, "WARDYN_RELAY_HOST_TARGET=10.0.0.9\n"
		if goos == "darwin" {
			file, want = p.plistFile, "<key>WARDYN_RELAY_HOST_TARGET</key>\n    <string>10.0.0.9</string>"
		}
		if got := f.read(file); !strings.Contains(got, want) || strings.Contains(got, "WARDYN_RELAY_LISTEN") {
			t.Errorf("%s: %s lacks %q or pins a native relay address:\n%s", goos, file, want, got)
		}
		g := newSetupFixture(t, goos)
		if _, err := g.run(setupOptions{tokenFile: g.tokenFile, relayHostTarget: "host-gateway"}); err != nil {
			t.Fatal(err)
		}
		gp, _ := g.host.paths()
		if got := g.read(map[string]string{"linux": gp.envFile, "darwin": gp.plistFile}[goos]); strings.Contains(got, "RELAY_HOST_TARGET") {
			t.Errorf("%s: the default target was written:\n%s", goos, got)
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
	for _, want := range []string{"<string>" + p.installed + "</string>", "<string>" + f.org + "</string>", "<key>WARDYN_RELAY_LISTEN</key>\n    <string>127.0.0.1</string>", "<string>" + p.logFile + "</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "__") || strings.Contains(plist, "DOCKER_HOST") || strings.Contains(plist, "<!--") {
		t.Errorf("plist keeps a placeholder, an empty DOCKER_HOST or the template comment:\n%s", plist)
	}
	if !contains(f.commands, "launchctl bootstrap gui/501 "+p.plistFile) {
		t.Errorf("agent not bootstrapped: %v", f.commands)
	}
	if !strings.Contains(out, "Docker Desktop detected") {
		t.Errorf("Docker Desktop default not announced:\n%s", out)
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
