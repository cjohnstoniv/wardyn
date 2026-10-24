// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// machineDependent matches what a setup prints from this computer's own measurements and the key it just made.
var machineDependent = regexp.MustCompile(`(?m)^(this computer reports |registered |  limits: ).*$|[0-9a-f]{64}`)

// TestSetupSuccessOutputGolden pins everything a first setup prints, with the temporary home and the
// organisation's address replaced by names.
func TestSetupSuccessOutputGolden(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := strings.NewReplacer(filepath.Dir(f.host.home), "ROOT", f.org, "ORG", strings.TrimPrefix(f.org, "http://"), "ORG").Replace(out)
	got = machineDependent.ReplaceAllString(got, "${1}MEASURED")
	want, err := os.ReadFile("testdata/setup_success.golden")
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile("testdata/setup_success.golden", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		want = []byte(got)
	} else if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("setup output differs from testdata/setup_success.golden:\n%s", got)
	}
}

// proxyTo is a proxy for the plain-http organisation of a setupFixture: handle answers each request it is
// sent, with the request already in proxy form.
func proxyTo(t *testing.T, f *setupFixture, handle http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.host.network.ProxyFor = func(*http.Request) (*url.URL, error) { return u, nil }
}

// stripsUpgrade forwards a request as a proxy that drops the WebSocket upgrade headers does.
func stripsUpgrade(w http.ResponseWriter, r *http.Request) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestSetupRefusesWithDoctorsMessageWhenTheWireFails pins that setup's preflight runs doctor's connection
// stages: a failing stage that makes setup pointless stops it, with the message doctor prints for it, before
// the organisation is asked to register anything.
func TestSetupRefusesWithDoctorsMessageWhenTheWireFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rig     func(*testing.T, *setupFixture)
		stage   string
		setting string
		in      string
	}{
		{"a proxy answers 407", func(t *testing.T, f *setupFixture) {
			proxyTo(t, f, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusProxyAuthRequired) })
		}, "healthz", "HTTP_PROXY", "answered 407: it wants proxy authentication"},
		{"healthz redirects", func(_ *testing.T, f *setupFixture) {
			f.healthz = func(w http.ResponseWriter) {
				w.Header().Set("Location", "https://login.example.com/")
				w.WriteHeader(http.StatusFound)
			}
		}, "healthz", "WARDYN_RUNNER_ORG_URL", "redirected (302) to login.example.com"},
		{"healthz answers 200 with a page", func(_ *testing.T, f *setupFixture) {
			f.healthz = func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>welcome</html>")) }
		}, "healthz", "WARDYN_RUNNER_ORG_URL", "not Wardyn's health report"},
		{"a gateway strips the WebSocket upgrade", func(_ *testing.T, f *setupFixture) { f.stripUpgrade = true },
			"wire", "WARDYN_RUNNER_ORG_URL", "answered 426 instead of switching to a WebSocket"},
		{"a proxy strips the WebSocket upgrade", func(t *testing.T, f *setupFixture) { proxyTo(t, f, stripsUpgrade) },
			"wire", "HTTP_PROXY", "answered 426 instead of switching to a WebSocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			tc.rig(t, f)
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			want := "the " + tc.stage + " stage failed, so setup stops here: "
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.in) ||
				!strings.Contains(err.Error(), "set "+tc.setting+" to") || !strings.Contains(err.Error(), "no token was spent") {
				t.Fatalf("err = %v, want %q naming %s and %q", err, want, tc.setting, tc.in)
			}
			if !strings.Contains(out, "FAIL  "+tc.stage) {
				t.Errorf("the output does not show the failed stage:\n%s", out)
			}
			if f.count("POST /api/v1/runners/register") != 0 || contains(f.events, "install") || contains(f.events, "register") || contains(f.events, "service") {
				t.Errorf("a refused setup still acted: calls %v, steps %v", f.calls, f.events)
			}
			if _, err := os.Stat(mustPaths(t, f).installed); err == nil {
				t.Error("a refused setup installed the binary")
			}
		})
	}
}

// TestSetupWarnsAndGoesOnWhenAStageThatDoesNotStopItFails pins the other half: the runner's last recorded
// failure is history to a setup that is about to restart the service, so it warns and registers.
func TestSetupWarnsAndGoesOnWhenAStageThatDoesNotStopItFails(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p := mustPaths(t, f)
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(offlineAt("dns", "lookup wardyn.example.com: no such host"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath(p.stateDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  status") || !strings.Contains(out, "warning: the status stage failed; setup goes on") {
		t.Errorf("the failed status stage is not shown as a warning:\n%s", out)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Errorf("setup did not register after the warning (%d)", f.count("POST /api/v1/runners/register"))
	}
}

// TestSetupWarnsOfAPinnedProxyImageOfAnotherRelease pins doctor's version-skew line in setup: the pinned image
// is a named choice, so the line is a warning and the registration goes on.
func TestSetupWarnsOfAPinnedProxyImageOfAnotherRelease(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: releaseProxyRepo + ":0.9.5"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  proxy-image") || !strings.Contains(out, "version skew: the pinned image "+releaseProxyRepo+":0.9.5 is release 0.9.5 but the organisation is release 0.10.0") ||
		!strings.Contains(out, "warning: the proxy-image stage failed; setup goes on") {
		t.Errorf("no skew warning:\n%s", out)
	}
	if f.count("POST /api/v1/runners/register") != 1 {
		t.Errorf("setup did not register after the warning (%d)", f.count("POST /api/v1/runners/register"))
	}
}

// TestSetupGoesOnWhenHealthzAnswersAPlainError pins that /healthz, which the runner never calls, does not stop
// a setup whose wire works: a gateway may route the API and answer nothing there. The line is a warning, and the
// registration goes on.
func TestSetupGoesOnWhenHealthzAnswersAPlainError(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := newSetupFixture(t, "linux")
			f.healthz = func(w http.ResponseWriter) { w.WriteHeader(code) }
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range []string{"FAIL  healthz", "ok    wire", "warning: the healthz stage failed; setup goes on"} {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in:\n%s", want, out)
				}
			}
			if f.count("POST /api/v1/runners/register") != 1 {
				t.Errorf("setup did not register (%d)", f.count("POST /api/v1/runners/register"))
			}
		})
	}
}

// connectProxy is a CONNECT proxy that tunnels to target whatever host it is asked for, and records each
// request. With wantAuth it answers 407 to a CONNECT that carries no Proxy-Authorization.
type connectProxy struct {
	URL  *url.URL
	mu   sync.Mutex
	seen []*http.Request
}

func newConnectProxy(t *testing.T, target string, wantAuth bool) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, r)
		p.mu.Unlock()
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		if wantAuth && r.Header.Get("Proxy-Authorization") == "" {
			w.Header().Set("Proxy-Authenticate", `Basic realm="corp"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		dst, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(dst, conn); _ = dst.Close() }()
		_, _ = io.Copy(conn, dst)
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p.URL = u
	return p
}

func (p *connectProxy) connects() (hosts []string, auth bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.seen {
		hosts = append(hosts, r.Host)
		auth = auth || r.Header.Get("Proxy-Authorization") != ""
	}
	return hosts, auth
}

// proxiedOrg is a setupFixture whose organisation is a TLS server reached as https://example.com:PORT, a name
// no machine resolves, so that only a proxy that tunnels to the server gets there. ca is the file that trusts it.
func proxiedOrg(t *testing.T) (f *setupFixture, ca, serverAddr string) {
	t.Helper()
	clearProxyEnv(t)
	f = newSetupFixture(t, "linux")
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	serverAddr = srv.Listener.Addr().String()
	f.org = "https://example.com:" + strings.Split(serverAddr, ":")[1]
	ca = writeFile(t, filepath.Join(t.TempDir(), "org-ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	return f, ca, serverAddr
}

// registerWithProxy makes f's computer a registered runner for f.org, whose runnerd.json carries proxy ("" for none),
// so that a setup run is a rerun: nothing registers, and the check is the only thing that dials out.
func registerWithProxy(t *testing.T, f *setupFixture, proxy string) setupPaths {
	t.Helper()
	p := mustPaths(t, f)
	key, err := runneridentity.Generate(p.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	id := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(f.org)}
	if err := runneridentity.Save(p.stateDir, id); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.configFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(runnerFile{OrgURL: f.org, RunnerProxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.configFile(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSetupChecksTheConnectionThroughTheProxyItInstalls pins that the stages route as the service will: a rerun
// from a shell with no proxy keeps the earlier setup's proxy (runnerd.json), and the check goes through it.
func TestSetupChecksTheConnectionThroughTheProxyItInstalls(t *testing.T) {
	f, ca, addr := proxiedOrg(t)
	proxy := newConnectProxy(t, addr, false)
	registerWithProxy(t, f, proxy.URL.String())
	out, err := f.run(setupOptions{orgCAFile: ca})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	hosts, _ := proxy.connects()
	if len(hosts) == 0 || hosts[0] != strings.TrimPrefix(f.org, "https://") {
		t.Errorf("the proxy was asked to connect to %v, want the organisation", hosts)
	}
	if want := "via proxy " + proxy.URL.Host; !strings.Contains(out, want) || !strings.Contains(out, "the settings setup will install for the service") {
		t.Errorf("the proxy stage does not say which proxy it used (%q):\n%s", want, out)
	}
}

// (a) The shell's credential for the proxy setup installs is the operator's own: the check sends it to that
// proxy, never prints it, never writes it to runnerd.json, and setup warns that the service needs the drop-in.
func TestSetupUsesTheShellCredentialForTheProxyItInstalls(t *testing.T) {
	f, ca, addr := proxiedOrg(t)
	proxy := newConnectProxy(t, addr, true)
	p := registerWithProxy(t, f, "")
	t.Setenv("HTTPS_PROXY", "http://deploy:s3cretpw@"+proxy.URL.Host)
	out, err := f.run(setupOptions{orgCAFile: ca})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if hosts, auth := proxy.connects(); len(hosts) == 0 || !auth {
		t.Errorf("proxy saw %v, credential sent: %v; want the check through it with the shell's credential", hosts, auth)
	}
	for name, text := range map[string]string{"output": out, "runnerd.json": f.read(p.configFile())} {
		if strings.Contains(text, "s3cretpw") || strings.Contains(text, "deploy") {
			t.Errorf("the credential is in the %s:\n%s", name, text)
		}
	}
	if !strings.Contains(out, "HTTPS_PROXY in this shell contains a credential") || !strings.Contains(out, "systemd drop-in") {
		t.Errorf("no warning that the service needs the drop-in:\n%s", out)
	}
}

// (b) A proxy that wants authentication, reached with no credential the shell supplies, stops setup at its 407,
// and the text names the drop-in recipe.
func TestSetupRefusesAProxy407ItHasNoCredentialFor(t *testing.T) {
	f, ca, addr := proxiedOrg(t)
	proxy := newConnectProxy(t, addr, true)
	registerWithProxy(t, f, proxy.URL.String())
	out, err := f.run(setupOptions{orgCAFile: ca})
	if err == nil {
		t.Fatalf("setup went on past a 407\n%s", out)
	}
	for _, want := range []string{"the tls stage failed, so setup stops here", "407: it wants proxy authentication", "set HTTPS_PROXY to", "systemd drop-in", "#a-runner-behind-a-corporate-proxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
}

// (c) A shell credential is sent only to the proxy setup installs: for another host or port the check uses
// the installed address, without it.
func TestServiceProxyForSendsAShellCredentialOnlyToTheInstalledProxy(t *testing.T) {
	org, _ := http.NewRequest(http.MethodGet, "https://example.com/healthz", nil)
	for _, tc := range []struct {
		name, installed, shell, wantHost, wantUser string
		used                                       bool
	}{
		{"same host and port", "http://proxy-a.example:3128", "http://u:p@PROXY-A.example:3128", "proxy-a.example:3128", "u", true},
		{"another host", "http://proxy-a.example:3128", "http://u:p@proxy-b.example:3128", "proxy-a.example:3128", "", false},
		{"another port", "http://proxy-a.example:3128", "http://u:p@proxy-a.example:8080", "proxy-a.example:3128", "", false},
		{"a shell address without a credential", "http://proxy-a.example:3128", "http://proxy-a.example:3128", "proxy-a.example:3128", "", false},
		{"no shell proxy", "http://proxy-a.example:3128", "", "proxy-a.example:3128", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, used := serviceProxyFor(runnerFile{RunnerProxy: tc.installed}, tc.shell)
			got, err := route(org)
			if err != nil || got == nil {
				t.Fatalf("route = %v, %v", got, err)
			}
			user := ""
			if got.User != nil {
				user = got.User.Username()
			}
			if used != tc.used || user != tc.wantUser || !strings.EqualFold(got.Host, tc.wantHost) {
				t.Errorf("route = host %q user %q used %v; want host %q user %q used %v", got.Host, user, used, tc.wantHost, tc.wantUser, tc.used)
			}
		})
	}
}

// (d) A service whose proxy credential is already in runnerd-proxy.env (the docs' recipe) gets a 407 as a warning:
// setup checks that the file exists, never opens it (it is mode 000 here), and goes on.
func TestSetupGoesOnPast407WhenTheCredentialFileExists(t *testing.T) {
	f, ca, addr := proxiedOrg(t)
	proxy := newConnectProxy(t, addr, true)
	p := registerWithProxy(t, f, proxy.URL.String())
	env := filepath.Join(filepath.Dir(p.configFile()), "runnerd-proxy.env")
	if err := os.WriteFile(env, []byte("HTTPS_PROXY=http://deploy:s3cretpw@"+proxy.URL.Host+"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(setupOptions{orgCAFile: ca})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "warning: the proxy answered 407 to this check. "+env+" exists") || !strings.Contains(out, "setup did not open it") {
		t.Errorf("no warning that the file supplies the credential:\n%s", out)
	}
	if !contains(f.commands, "systemctl --user enable --now wardyn-runnerd.service") {
		t.Errorf("setup did not go on to start the service: %v", f.commands)
	}
	if strings.Contains(out, "s3cretpw") {
		t.Errorf("the credential is in the output:\n%s", out)
	}
}

// TestSetupNamesWhereTheOrganisationCameFrom pins the managed preference as the source the org-url line names.
func TestSetupNamesWhereTheOrganisationCameFrom(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, orgFrom: "the OrgURL managed preference"})
	if err != nil || !strings.Contains(out, "(from the OrgURL managed preference)") {
		t.Fatalf("err=%v\n%s", err, out)
	}
}
