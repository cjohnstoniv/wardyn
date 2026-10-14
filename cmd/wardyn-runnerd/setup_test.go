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
	relayErr    error       // what the engine check answers, when set
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
		docker: f.docker}
	if err := os.MkdirAll(f.host.home, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// docker answers as prepareSetupDocker would on a native engine (its proxy reaches the relay through
// host-gateway), on Docker Desktop (the same), or on a VM-backed engine reached at 192.168.5.2. An
// explicit target always reaches. It applies the real bind rule and candidate list.
func (f *setupFixture) docker(_ context.Context, image, network string, want relayPair) (dockerEngine, relayPair, error) {
	f.networks = append(f.networks, network)
	f.checks = append(f.checks, image+" "+network)
	f.wants = append(f.wants, want)
	if f.relayErr != nil {
		return dockerEngine{}, relayPair{}, f.relayErr
	}
	gw := netip.MustParseAddr("172.17.0.1")
	eng, reach := dockerEngine{desc: `"Ubuntu 24.04 LTS" (laptop)`, native: true, gateway: gw, bridge: []netip.Addr{gw}}, hostGateway
	switch {
	case f.desktop:
		eng = dockerEngine{desc: `"Docker Desktop" (docker-desktop)`, why: "Docker Desktop"}
	case f.vm:
		eng, reach = dockerEngine{desc: `"Alpine Linux v3.21" (lima-rancher-desktop)`, why: "its bridge interface is not on this host"}, "192.168.5.2"
	}
	if want.bind.IsValid() {
		if err := admitRelayBind(want.bind, eng); err != nil {
			return dockerEngine{}, relayPair{}, err
		}
	}
	for _, p := range relayCandidates(eng, want) {
		if p.target == reach || p.target == want.target {
			return eng, p, nil
		}
	}
	return dockerEngine{}, relayPair{}, errors.New("relay: no container on Docker engine " + eng.desc + " reached this runner")
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
		"root":                                   {setupOptions{}, func(f *setupFixture) { f.host.uid = 0 }, "not as root"},
		"no token file":                          {setupOptions{}, nil, "no runner identity yet"},
		"missing token file":                     {setupOptions{tokenFile: "/nonexistent/token"}, nil, "token file not found"},
		"bad relay address":                      {setupOptions{relayListen: "not-an-address"}, nil, "--relay-listen"},
		"default without pool":                   {setupOptions{claim: true, makeDefault: true}, nil, "--default needs --pool"},
		"pool without claim":                     {setupOptions{pool: "Laptop"}, nil, "--pool needs --claim"},
		"unsafe setting":                         {setupOptions{network: "net work"}, nil, "unsupported characters"},
		"docker unusable":                        {setupOptions{}, func(f *setupFixture) { f.relayErr = os.ErrPermission }, "permission denied"},
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
		file, wants := p.envFile, []string{"WARDYN_RELAY_LISTEN=127.0.0.1\n", "WARDYN_RELAY_HOST_TARGET=192.168.5.2\n"}
		if goos == "darwin" {
			file, wants = p.plistFile, []string{"<key>WARDYN_RELAY_LISTEN</key>\n    <string>127.0.0.1</string>", "<key>WARDYN_RELAY_HOST_TARGET</key>\n    <string>192.168.5.2</string>"}
		}
		got := f.read(file)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %s lacks %q:\n%s", goos, file, want, got)
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
		dwant := map[string]string{"linux": "WARDYN_RELAY_HOST_TARGET=host-gateway\n", "darwin": "<key>WARDYN_RELAY_HOST_TARGET</key>\n    <string>host-gateway</string>"}[goos]
		if got := d.read(map[string]string{"linux": dp.envFile, "darwin": dp.plistFile}[goos]); !strings.Contains(got, dwant) {
			t.Errorf("%s: a VM-backed engine's host-gateway target was not pinned:\n%s", goos, got)
		}

		n := newSetupFixture(t, goos)
		if _, err := n.run(setupOptions{tokenFile: n.tokenFile}); err != nil {
			t.Fatal(err)
		}
		np, _ := n.host.paths()
		got = n.read(map[string]string{"linux": np.envFile, "darwin": np.plistFile}[goos])
		if strings.Contains(got, "WARDYN_RELAY_") || strings.Contains(got, "__") {
			t.Errorf("%s: a native engine's relay was pinned, or a placeholder was left:\n%s", goos, got)
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
		file, want := p.envFile, "WARDYN_RELAY_HOST_TARGET=10.0.0.9\n"
		if goos == "darwin" {
			file, want = p.plistFile, "<key>WARDYN_RELAY_HOST_TARGET</key>\n    <string>10.0.0.9</string>"
		}
		if got := f.read(file); !strings.Contains(got, want) || strings.Contains(got, "WARDYN_RELAY_LISTEN") {
			t.Errorf("%s: %s lacks %q or pins a native relay address:\n%s", goos, file, want, got)
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
