// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, err := os.Stat(p.envFile); err == nil {
		t.Error("a refused setup wrote the env file")
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
	if env := g.read(gp.envFile); !slices.Contains(g.checks, "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1-rc.2 wardyn-internal") || strings.Contains(env, "WARDYN_PROXY_IMAGE") {
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
	if env := f.read(mustPaths(t, f).envFile); !strings.Contains(env, "WARDYN_PROXY_IMAGE="+mirror+"\n") {
		t.Errorf("the env file lacks the organisation's image:\n%s", env)
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
	if env := h.read(mustPaths(t, h).envFile); !strings.Contains(env, "WARDYN_PROXY_IMAGE=ghcr.io/example/proxy:1\n") {
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
	if env := f.read(mustPaths(t, f).envFile); !strings.Contains(env, "WARDYN_PROXY_IMAGE="+mirror+"\n") {
		t.Errorf("the env file lacks the organisation's image:\n%s", env)
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
	if env := f.read(mustPaths(t, f).envFile); strings.Contains(env, "WARDYN_PROXY_IMAGE") || strings.Contains(env, "ExecStart") {
		t.Errorf("the env file pins the derived release image or the answer:\n%s", env)
	}

	g := newSetupFixture(t, "linux")
	g.flagless, g.answered, g.unpullable = true, "evil\nExecStart=/bin/sh", release
	if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err == nil || !strings.Contains(err.Error(), "unsupported characters") || !strings.Contains(err.Error(), "no new token is needed") {
		t.Fatalf("unsafe answered image with nothing probed: %v", err)
	}
	if _, serr := os.Stat(mustPaths(t, g).envFile); serr == nil {
		t.Error("an unsafe image reached the env file")
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
		capped bool
		want   int
	}{{false, 1}, {true, 0}} {
		engineDiskCapped = func(context.Context) (bool, bool) { return tc.capped, true }
		f := newSetupFixture(t, "linux")
		out, err := f.run(setupOptions{tokenFile: f.tokenFile})
		if n := strings.Count(out, noDiskCapLine); err != nil || n != tc.want {
			t.Errorf("capped=%v: the no-cap line appeared %d times, want %d (%v)\n%s", tc.capped, n, tc.want, err, out)
		}
	}
}
