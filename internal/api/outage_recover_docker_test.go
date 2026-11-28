// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/storehealth"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ordAllowedHost is the one name both runs may reach: under the site's internal_hosts suffix, mapped in the
	// proxy to the CGNAT address where the test's own httpd answers.
	ordAllowedHost = "svc.corp.internal"
	// ordPausedBody is the proxy's refusal while its run's network is paused (internal/egress/proxy/self_lapse.go
	// pausedBody).
	ordPausedBody = "wardyn: this run's network is paused until Wardyn can check the run again"
	// ordClosedBody is the proxy's refusal once its gate has lapsed, as a strict run's does at its allowance
	// (self_lapse.go refuseClosed).
	ordClosedBody = "wardyn: this run's egress is closed"
	// ordStrictHint is the failure hint of a run its strict outage terms end (run_lost.go sweepLapsedRunTokens).
	ordStrictHint = "the run's strict outage terms end it: its network was not confirmed within its allowance"
	// ordControlHost is the name a proxy reaches the host by: the driver maps it to the Docker host gateway in
	// every proxy it starts (prepareProxy), and the test's control-plane listener serves a certificate for it.
	ordControlHost = "host.docker.internal"
)

// TestOutageRecover_ServerDocker_Docker is the live tier of the outage recovery walk (#4082) on the server's own
// Docker: a real wardynd Server (its real sweeps, its real store health tracking, its control-plane heartbeat)
// over a Postgres the test owns, dispatching runs onto the real Docker driver, which starts the real wardyn-proxy
// (built from this tree). Two runs are started: A on the default outage terms and B on a policy outage of
// {allowance_minutes: 30, strict: true}. Each is RUNNING with a renew behind it before the test stops its Postgres
// for WARDYN_OUTAGE_STOP_MIN minutes (default 70). Asserted:
//   - at minute 35, A's allowed host answers through A's proxy and B's does not;
//   - at minute 65, A's proxy answers 503 with the paused body;
//   - within 120 s of the Postgres starting again, A's allowed host answers again and A has a run.token.recover
//     audit row, and B is FAILED with the strict-terms failure hint.
//
// Everything it uses it owns and removes: its own Postgres container (wardyn-outage-pg-<tag>), its own networks,
// its allowed-host httpd, and the proxy, agent and per-run network of each run, found by the deployment label
// the driver stamps (the name of the test's control-plane network). It never touches the resident compose stack.
// Needs busybox:latest and postgres:17 (pulled when absent); the proxy and the agent are busybox, the proxy with
// the binary built here. It runs for about WARDYN_OUTAGE_STOP_MIN plus up to 25 minutes for each run's first
// renew, so give it -timeout 120m.
func TestOutageRecover_ServerDocker_Docker(t *testing.T) {
	testfloor.Mark(t, "outage-recover-docker")
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the live outage recovery walk")
	}
	stopMin := ordStopMinutes(t)
	tag := uuid.NewString()[:8]
	ctx := context.Background()

	// Cleanups run last-in first: the server stops first, then the run objects, then Postgres and the networks.
	ctlNet := "wardyn-outage-net-" + tag
	dockerCLI(t, "network", "create", ctlNet)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", ctlNet).Run() })

	cgnatIP := ordCGNATNetwork(t, tag)
	httpPort := ordFreePort(t)
	httpd := "wardyn-outage-httpd-" + tag
	dockerCLI(t, "run", "-d", "--name", httpd, "--network", "host", "busybox:latest",
		"sh", "-c", "mkdir -p /w && echo internal > /w/index.html && exec httpd -f -p "+httpPort+" -h /w")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", httpd).Run() })

	pgName, dsn := ordStartPostgres(t, tag)
	pool := ordConnectPostgres(t, dsn)
	t.Cleanup(pool.Close)
	pg := store.NewPG(pool)
	// A deployment with a runner wired places every run through a pool, and a run naming none takes the
	// organisation's default: the same default pool every Postgres fixture with a runner seeds.
	seedRunPool(t, pool)
	if _, err := pg.PutSiteConfig(ctx, types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}); err != nil {
		t.Fatalf("put the site config: %v", err)
	}

	// The proxy under test is this tree's, static, for the daemon's platform, run in a busybox image.
	proxyBin := filepath.Join(t.TempDir(), "wardyn-proxy")
	build := exec.Command("go", "build", "-o", proxyBin, "github.com/cjohnstoniv/wardyn/cmd/wardyn-proxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wardyn-proxy: %v\n%s", err, out)
	}
	sub, err := docker.New(docker.Config{
		ProxyImage: "busybox:latest", ProxyBinaryHostPath: proxyBin, InternalNetwork: ctlNet,
		TestProxyExtraHosts: []string{ordAllowedHost + ":" + cgnatIP},
	})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}

	// The proxy-facing hop: the server's internal handler over TLS from the test's own CA, on every address of
	// the host so a proxy reaches it at the Docker host gateway.
	caBlob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServingCert(ordControlHost, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "0.0.0.0:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen for the proxies: %v", err)
	}

	baseCtx, cancelBase := context.WithCancel(ctx)
	h := newHarness(t)
	ageID, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, ageID)
	if err != nil {
		t.Fatal(err)
	}
	health := storehealth.New(time.Now, 25*time.Minute) // WARDYN_LAPSE_RECOVERY_GRACE's default
	go health.Run(baseCtx, pg.Ping)
	cfg := baseTestConfig(h, pg)
	cfg.BaseCtx = baseCtx
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.Secrets = sec
	cfg.Audit = store.Recorder{Pool: pool}
	cfg.Runner = orchestrator.New(sub).WithRefStore(pg)
	cfg.AgentImages = map[string]string{"claude-code": "busybox:latest"}
	cfg.StoreHealth = health
	cfg.ControlPlaneURL = "https://" + ordControlHost + ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	cfg.ControlPlaneCAPEM = string(ca.CertPEM)
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC1}
	srv := New(cfg)
	hop := &http.Server{Handler: srv.InternalHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hop.Serve(ln) }()

	var runIDs []uuid.UUID
	t.Cleanup(func() { ordRemoveRunObjects(ctlNet, runIDs) })
	t.Cleanup(func() {
		cancelBase()
		_ = hop.Close()
		srv.WaitBackground()
	})
	if err := srv.ReconcileOnBoot(baseCtx); err != nil {
		t.Logf("boot reconcile: %v", err)
	}

	create := func(policy string) uuid.UUID {
		t.Helper()
		body := `{"agent":"claude-code","interactive":true,"inline_policy":` + policy + `}`
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST /runs = %d %s", w.Code, w.Body.String())
		}
		var created createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, created.ID)
		return created.ID
	}
	const common = `"min_confinement_class":"CC1","auto_stop_after_sec":-1,"allowed_domains":["` + ordAllowedHost + `"]`
	runA := create(`{` + common + `}`)
	runB := create(`{` + common + `,"outage":{"allowance_minutes":30,"strict":true}}`)
	for name, id := range map[string]uuid.UUID{"A": runA, "B": runB} {
		ordWaitRunningRenewed(t, pg, pool, name, id)
	}

	// Before the cut both runs reach the allowed host through their own proxies.
	agentA, agentB := docker.SandboxRef(runA), docker.SandboxRef(runB)
	for name, agent := range map[string]string{"A": agentA, "B": agentB} {
		if code, body := ordFetch(agent, httpPort); code != http.StatusOK || !strings.Contains(body, "internal") {
			t.Fatalf("before the cut, run %s's allowed host answered %d %q, want 200 internal", name, code, body)
		}
	}

	// The cut: Postgres stops and stays stopped for stopMin minutes; the server and the proxies keep running.
	stopAt := time.Now()
	dockerCLI(t, "stop", pgName)
	t.Logf("postgres %s stopped; the cut lasts %d minutes", pgName, stopMin)
	first, second := min(35, stopMin/2), max(stopMin-5, 1)
	ordSleepUntil(stopAt.Add(time.Duration(first) * time.Minute))
	if code, body := ordFetch(agentA, httpPort); code != http.StatusOK || !strings.Contains(body, "internal") {
		t.Errorf("minute %d: run A's allowed host answered %d %q, want 200 internal (the default terms keep its network for an hour)", first, code, body)
	}
	// Its proxy must still be up and refusing: no answer at all would be a dead proxy, which proves nothing.
	if code, body := ordFetch(agentB, httpPort); code != http.StatusServiceUnavailable || !strings.Contains(body, ordClosedBody) {
		t.Errorf("minute %d: run B's proxy answered %d %q, want 503 with %q (its strict 30-minute allowance is spent)", first, code, body, ordClosedBody)
	}
	ordSleepUntil(stopAt.Add(time.Duration(second) * time.Minute))
	if code, body := ordFetch(agentA, httpPort); code != http.StatusServiceUnavailable || !strings.Contains(body, ordPausedBody) {
		t.Errorf("minute %d: run A's proxy answered %d %q, want 503 with %q", second, code, body, ordPausedBody)
	}
	ordSleepUntil(stopAt.Add(time.Duration(stopMin) * time.Minute))

	// Postgres returns. Everything below is read through the test's own pool, which reconnects by itself.
	startAt := time.Now()
	dockerCLI(t, "start", pgName)
	var aAnswers, aRecovered, bEnded bool
	var aCode int
	var aBody, bState, bHint string
	ordPoll(startAt.Add(120*time.Second), 3*time.Second, func() bool {
		if !aAnswers {
			aCode, aBody = ordFetch(agentA, httpPort)
			aAnswers = aCode == http.StatusOK && strings.Contains(aBody, "internal")
		}
		if !aRecovered {
			if evs, err := pg.QueryAuditEvents(ctx, runA, 0); err == nil {
				for _, ev := range evs {
					if ev.Action == "run.token.recover" {
						aRecovered = true
					}
				}
			}
		}
		if !bEnded {
			if run, err := pg.GetRun(ctx, runB); err == nil {
				bState, bHint = string(run.State), run.FailureHint
				bEnded = run.State == types.RunFailed
			}
		}
		return aAnswers && aRecovered && bEnded
	})
	if !aAnswers {
		t.Errorf("within 120 s of Postgres returning, run A's allowed host answered %d %q, want 200 internal", aCode, aBody)
	}
	if !aRecovered {
		t.Errorf("within 120 s of Postgres returning, run A has no run.token.recover audit row")
	}
	if !bEnded {
		t.Errorf("within 120 s of Postgres returning, run B is %q (hint %q), want FAILED", bState, bHint)
	} else if !strings.Contains(bHint, ordStrictHint) {
		t.Errorf("run B failed with hint %q, want %q", bHint, ordStrictHint)
	}
	if run, err := pg.GetRun(ctx, runA); err != nil || run.State != types.RunRunning {
		t.Errorf("run A after the recovery: %q %v, want RUNNING", run.State, err)
	}
}

// ordStopMinutes is WARDYN_OUTAGE_STOP_MIN, 70 when unset.
func ordStopMinutes(t *testing.T) int {
	t.Helper()
	v := os.Getenv("WARDYN_OUTAGE_STOP_MIN")
	if v == "" {
		return 70
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		t.Fatalf("WARDYN_OUTAGE_STOP_MIN=%q, want a whole number of minutes, at least 2", v)
	}
	return n
}

// ordFreePort is a TCP port nothing listened on a moment ago.
func ordFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// ordCGNATNetwork creates a bridge network over a random 100.64.x.0/24 and returns its gateway address, a host
// address the httpd answers on (a host-network container listens on every host address) and in none of a proxy's
// interface subnets. The network is removed when the test ends.
func ordCGNATNetwork(t *testing.T, tag string) string {
	t.Helper()
	var lastErr error
	for range 20 {
		octet := strconv.Itoa(1 + rand.IntN(254))
		name, gw := "wardyn-outage-cgnat-"+tag, "100.64."+octet+".1"
		out, err := exec.Command("docker", "network", "create", "--subnet", "100.64."+octet+".0/24", "--gateway", gw, name).CombinedOutput()
		if err == nil {
			t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
			return gw
		}
		lastErr = fmt.Errorf("%v: %s", err, out)
	}
	t.Fatalf("no free 100.64.x.0/24 for the allowed host's network: %v", lastErr)
	return ""
}

// ordStartPostgres runs the test's own Postgres on a loopback port, kept across a stop and start, and returns its
// container name and DSN. The superuser password is made for this run and used on nothing else.
func ordStartPostgres(t *testing.T, tag string) (name, dsn string) {
	t.Helper()
	port, pw := ordFreePort(t), uuid.NewString()
	name = "wardyn-outage-pg-" + tag
	dockerCLI(t, "run", "-d", "--name", name,
		"-e", "POSTGRES_USER=wardyn", "-e", "POSTGRES_PASSWORD="+pw, "-e", "POSTGRES_DB=wardyn",
		"-p", "127.0.0.1:"+port+":5432", "postgres:17")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	return name, "postgres://wardyn:" + pw + "@127.0.0.1:" + port + "/wardyn?sslmode=disable"
}

// ordConnectPostgres waits for the fresh Postgres to accept connections, connects and migrates.
func ordConnectPostgres(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	var pool *pgxpool.Pool
	var err error
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pool, err = db.Connect(cctx, dsn)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never accepted a connection: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// ordWaitRunningRenewed waits for run id to be RUNNING with one renew behind it: its token_renewed_at moved past
// its creation. A proxy renews at least every 11 minutes, so 25 is generous. A run that ends first fails the test
// with its hint.
func ordWaitRunningRenewed(t *testing.T, pg store.PG, pool *pgxpool.Pool, name string, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var state types.RunState
	var hint string
	ok := ordPoll(time.Now().Add(25*time.Minute), 5*time.Second, func() bool {
		run, err := pg.GetRun(ctx, id)
		if err != nil {
			return false
		}
		state, hint = run.State, run.FailureHint
		if run.State.IsTerminal() {
			t.Fatalf("run %s ended before the cut: %s %q", name, run.State, run.FailureHint)
		}
		var renewed bool
		if run.State != types.RunRunning ||
			pool.QueryRow(ctx, `SELECT token_renewed_at > created_at + interval '2 seconds' FROM agent_runs WHERE id = $1`, id).Scan(&renewed) != nil {
			return false
		}
		return renewed
	})
	if !ok {
		t.Fatalf("run %s is %s %q: not RUNNING with a renew behind it after 25 minutes", name, state, hint)
	}
}

// ordPoll calls cond every interval until it returns true or deadline passes, and says which.
func ordPoll(deadline time.Time, every time.Duration, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(every)
	}
}

func ordSleepUntil(at time.Time) { time.Sleep(time.Until(at)) }

// ordFetch asks agent, a run's agent container, for the allowed host through its own proxy, as an absolute-form
// GET over the agent's busybox nc so the status line and the body both come back, and returns the status (0 when
// nothing answered) and the body.
func ordFetch(agent, port string) (int, string) {
	script := fmt.Sprintf(`printf 'GET http://%[1]s:%[2]s/ HTTP/1.1\r\nHost: %[1]s:%[2]s\r\nConnection: close\r\n\r\n' | nc -w 10 wardyn-proxy %[3]d`,
		ordAllowedHost, port, runner.ProxyListenPort)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "docker", "exec", agent, "sh", "-c", script).Output()
	head, body, _ := strings.Cut(string(out), "\r\n\r\n")
	line, _, _ := strings.Cut(head, "\r\n")
	if f := strings.Fields(line); len(f) >= 2 {
		code, _ := strconv.Atoi(f[1])
		return code, body
	}
	return 0, string(out)
}

// ordRemoveRunObjects removes what the driver made for the test's runs: every container, network and volume
// labelled with the test's deployment (the control-plane network's name), then each run's per-run network by
// name in case it carries no label. Errors are ignored: a test that failed early may have made none.
func ordRemoveRunObjects(deployment string, runs []uuid.UUID) {
	filter := "label=wardyn.deployment=" + deployment
	for _, kind := range []struct{ list, rm []string }{
		{[]string{"ps", "-aq"}, []string{"rm", "-f", "-v"}},
		{[]string{"network", "ls", "-q"}, []string{"network", "rm"}},
		{[]string{"volume", "ls", "-q"}, []string{"volume", "rm", "-f"}},
	} {
		out, err := exec.Command("docker", append(kind.list, "--filter", filter)...).Output()
		if ids := strings.Fields(string(out)); err == nil && len(ids) > 0 {
			_ = exec.Command("docker", append(kind.rm, ids...)...).Run()
		}
	}
	for _, id := range runs {
		_ = exec.Command("docker", "rm", "-f", "-v", "wardyn-agent-"+id.String(), "wardyn-proxy-"+id.String()).Run()
		_ = exec.Command("docker", "network", "rm", "wardyn-int-"+id.String()).Run()
	}
}
