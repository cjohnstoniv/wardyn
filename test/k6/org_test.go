// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

// TestK6Org is the half of the suite that needs an organisation: scripts/k6-kind-conformance.sh
// stands up a real wardynd and postgres in the cluster, enrols the runnerd chart as an organisation
// runner through the org's own mint and claim, and runs this test against both while runnerd is live.
// Each case drives the org's API as a security operator would and observes the cluster under the
// admin kubeconfig; node-level steps (a stopped kubelet, a pod's network namespace) go through the
// kind node containers on the docker socket.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// orgEnv is what the script hands TestK6Org.
type orgEnv struct {
	admin                        kubernetes.Interface
	adminKC                      string
	runsNS, ownNS, orgNS, deploy string
	runnerID, relayIP, schedNode string
	adminToken, secToken, orgURL string
	orgHost, apiserver, probe    string
	personalPool                 string
	http                         *http.Client
	poolID                       string
	// state the later cases continue from
	residentRun string // walk-trust-placement's run holding a runner_resident value
	fenced      *fencedProxy
	lapse       *lapseRun
}

// lapseRun is self-lapse-end-to-end's run: renewed once, then ended at the org behind runnerd's back.
type lapseRun struct {
	id     string
	marked time.Time
	err    string
}

// fencedProxy is fence-live-http2's run, fenced while its node's kubelet is stopped so its proxy lives on.
type fencedProxy struct{ id, node, pid string }

func newOrgEnv(t *testing.T) *orgEnv {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", must(t, "K6_ADMIN_KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	o := &orgEnv{
		admin: kubernetes.NewForConfigOrDie(cfg), adminKC: must(t, "K6_ADMIN_KUBECONFIG"),
		runsNS: must(t, "K6_RUNS_NS"), ownNS: must(t, "K6_OWN_NS"), orgNS: must(t, "K6_ORG_NS"), deploy: must(t, "K6_RUNNERD_DEPLOY"),
		runnerID: must(t, "K6_RUNNER_ID"), relayIP: must(t, "K6_RELAY_IP"), schedNode: must(t, "K6_SCHED_NODE"),
		adminToken: must(t, "K6_ORG_ADMIN_TOKEN"), secToken: must(t, "K6_ORG_SEC_TOKEN"), orgURL: must(t, "K6_ORG_URL"),
		apiserver: must(t, "K6_APISERVER"), probe: must(t, "K6_AGENTPROBE"), personalPool: os.Getenv("K6_PERSONAL_POOL"),
	}
	u, err := url.Parse(o.orgURL)
	if err != nil {
		t.Fatal(err)
	}
	o.orgHost = u.Hostname()
	pem, err := os.ReadFile(must(t, "K6_ORG_CA"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	addr := must(t, "K6_ORG_ADDR") // the org Service's node port on the control plane: the suite runs outside the cluster
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	o.http = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: o.orgHost, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}}
	return o
}

// api calls the org as token and decodes a JSON answer into out (when non-nil); it returns the
// status and the raw body.
func (o *orgEnv) api(t *testing.T, token, method, path string, body any, out any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, ok := body.([]byte)
		if !ok {
			if s, isStr := body.(string); isStr {
				b = []byte(s)
			} else {
				var err error
				if b, err = json.Marshal(body); err != nil {
					t.Fatal(err)
				}
			}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://"+o.orgHost+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, strings.TrimSpace(string(raw))
}

// kubectl runs kubectl under the admin kubeconfig.
func (o *orgEnv) kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return sh(t, "kubectl", append([]string{"--kubeconfig", o.adminKC}, args...)...)
}

// node runs a shell line on a kind node container.
func node(t *testing.T, name, script string) string {
	t.Helper()
	return sh(t, "docker", "exec", name, "sh", "-c", script)
}

// pool is the org pool holding the org runner, created on first use.
func (o *orgEnv) pool(t *testing.T) string {
	t.Helper()
	if o.poolID != "" {
		return o.poolID
	}
	var p struct {
		ID string `json:"id"`
	}
	if code, raw := o.api(t, o.secToken, http.MethodPost, "/api/v1/runner-pools", map[string]any{"name": "k6-org-" + short()}, &p); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create pool = %d %s", code, raw)
	}
	if code, raw := o.api(t, o.secToken, http.MethodPut, "/api/v1/runner-pools/"+p.ID+"/runners/"+o.runnerID, nil, nil); code >= 300 {
		t.Fatalf("add runner to pool = %d %s", code, raw)
	}
	o.poolID = p.ID
	return p.ID
}

// orgRun is the part of a run row the cases read.
type orgRun struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	RunnerID   string `json:"runner_id"`
	SandboxRef string `json:"sandbox_ref"`
}

// launch starts a run whose agent runs task (a shell line, see the harness's agent-run) on the org
// runner and returns its id; extra fields join the request.
func (o *orgEnv) launch(t *testing.T, task string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"agent": "claude-code", "task": task, "placement": "local", "runner_pool_id": o.pool(t), "inline_policy": orgPolicy(nil)}
	for k, v := range extra {
		body[k] = v
	}
	var r orgRun
	code, raw := o.api(t, o.secToken, http.MethodPost, "/api/v1/runs", body, &r)
	if code != http.StatusCreated && code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("POST /runs = %d %s", code, raw)
	}
	t.Cleanup(func() { o.api(t, o.secToken, http.MethodPost, "/api/v1/runs/"+r.ID+"/kill", nil, nil) })
	return r.ID
}

func (o *orgEnv) run(t *testing.T, id string) orgRun {
	t.Helper()
	var r orgRun
	if code, raw := o.api(t, o.secToken, http.MethodGet, "/api/v1/runs/"+id, nil, &r); code != http.StatusOK {
		t.Fatalf("GET run = %d %s", code, raw)
	}
	return r
}

// waitState waits until the run is in one of states.
func (o *orgEnv) waitState(t *testing.T, id string, within time.Duration, states ...string) orgRun {
	t.Helper()
	var r orgRun
	deadline := time.Now().Add(within)
	for {
		r = o.run(t, id)
		for _, s := range states {
			if r.State == s {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s: state %s, want one of %v within %s", id, r.State, states, within)
		}
		time.Sleep(2 * time.Second)
	}
}

// live launches a long-running run and waits for its proxy and agent pods to run.
func (o *orgEnv) live(t *testing.T, extra map[string]any) string {
	t.Helper()
	id := o.launch(t, "sleep 3000", extra)
	o.waitState(t, id, 4*time.Minute, "RUNNING")
	for _, pod := range []string{"wardyn-proxy-" + id, "wardyn-agent-" + id} {
		o.kubectl(t, "-n", o.runsNS, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=180s")
	}
	return id
}

// agent runs argv in the run's agent container.
func (o *orgEnv) agent(t *testing.T, id string, argv ...string) string {
	t.Helper()
	out, _ := exec.Command("kubectl", append([]string{"--kubeconfig", o.adminKC, "-n", o.runsNS, "exec", "wardyn-agent-" + id, "-c", "agent", "--"}, argv...)...).CombinedOutput()
	return string(out)
}

// proxyNode and proxyPID name the node holding the run's proxy pod and the PID of its sandbox, whose
// network namespace a node-side probe enters (nsenter -t PID -n) to dial as the proxy.
func (o *orgEnv) proxyNetns(t *testing.T, id string) (string, string) {
	t.Helper()
	nodeName := strings.TrimSpace(o.kubectl(t, "-n", o.runsNS, "get", "pod", "wardyn-proxy-"+id, "-o", "jsonpath={.spec.nodeName}"))
	pid := strings.TrimSpace(node(t, nodeName, "crictl inspectp -o go-template --template '{{.info.pid}}' $(crictl pods --name wardyn-proxy-"+id+" -q | head -1)"))
	if pid == "" {
		t.Fatalf("no sandbox pid for proxy of %s on %s", id, nodeName)
	}
	return nodeName, pid
}

// orgPolicy is a run's inline policy: no grant unless one is passed, the named hosts allowed, every
// other host refused without a prompt.
func orgPolicy(grants []map[string]any, allowed ...string) map[string]any {
	p := map[string]any{"allowed_domains": append([]string{"example.com"}, allowed...), "first_use_approval": "always_deny",
		"min_confinement_class": "CC1", "auto_stop_after_sec": -1}
	if grants != nil {
		p["eligible_grants"] = grants
	}
	return p
}

// residentGrant is an env_secret grant of the org secret k6-op-secret, delivered as RES_TOKEN.
var residentGrant = []map[string]any{{"kind": "env_secret", "scope": map[string]any{"name": "RES_TOKEN", "secret_name": "k6-op-secret"}, "requires_approval": false}}

// connect is agentprobe connect from the run's agent: the proxy's status line and body.
func (o *orgEnv) connect(t *testing.T, id, target string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "exec", "wardyn-agent-"+id, "-c", "agent", "--",
		"timeout", "30", "agentprobe", "connect", target).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// decisions is the run's proxy decision log as host -> the decisions taken for it.
func (o *orgEnv) decisions(t *testing.T, id string) map[string][]string {
	t.Helper()
	out, _ := exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "logs", "wardyn-proxy-"+id, "-c", "wardyn-proxy").Output()
	got := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		var d struct {
			Request    struct{ Host string } `json:"request"`
			Decision   string                `json:"decision"`
			RuleSource string                `json:"rule_source"`
		}
		if json.Unmarshal([]byte(line), &d) == nil && d.Decision != "" {
			got[d.Request.Host] = append(got[d.Request.Host], d.Decision+":"+d.RuleSource)
		}
	}
	return got
}

// audit is the org's audit rows for a run.
type auditRow struct {
	Action  string          `json:"action"`
	Target  string          `json:"target"`
	Outcome string          `json:"outcome"`
	Data    json.RawMessage `json:"data"`
}

func (o *orgEnv) audit(t *testing.T, runID string) []auditRow {
	t.Helper()
	var rows []auditRow
	if code, raw := o.api(t, o.secToken, http.MethodGet, "/api/v1/audit?run_id="+runID+"&limit=500", nil, &rows); code != http.StatusOK {
		t.Fatalf("GET audit = %d %s", code, raw)
	}
	return rows
}

// waitAudit waits for an audit row of the run that match accepts.
func (o *orgEnv) waitAudit(t *testing.T, runID string, within time.Duration, what string, match func(auditRow) bool) auditRow {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, r := range o.audit(t, runID) {
			if match(r) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s: no audit row %s within %s", runID, what, within)
		}
		time.Sleep(2 * time.Second)
	}
}

// workers are the kind worker nodes, sorted.
func (o *orgEnv) workers(t *testing.T) []string {
	t.Helper()
	out := o.kubectl(t, "get", "nodes", "-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[*].metadata.name}")
	ns := strings.Fields(out)
	slices.Sort(ns)
	return ns
}

// sched puts the sandbox scheduling label on node only.
func (o *orgEnv) sched(t *testing.T, node string) {
	t.Helper()
	for _, n := range o.workers(t) {
		if n == node {
			o.kubectl(t, "label", "node", n, schedLabel+"=on", "--overwrite")
		} else {
			o.kubectl(t, "label", "node", n, schedLabel+"-")
		}
	}
}

// kubelet stops or starts the kubelet of a kind node.
func kubelet(t *testing.T, nodeName string, on bool) {
	t.Helper()
	verb := "stop"
	if on {
		verb = "start"
	}
	node(t, nodeName, "systemctl "+verb+" kubelet")
}

// runnerdIP is runnerd's current pod IP.
func (o *orgEnv) runnerdIP(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(o.kubectl(t, "-n", o.ownNS, "get", "pod", "-l", "app.kubernetes.io/instance=k6,app.kubernetes.io/component=runnerd",
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].status.podIP}"))
}

// online waits for the org to show the runner online.
func (o *orgEnv) online(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var rn struct {
			Online bool `json:"online"`
		}
		o.api(t, o.secToken, http.MethodGet, "/api/v1/runners/"+o.runnerID, nil, &rn)
		if rn.Online {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner not online within %s", within)
		}
		time.Sleep(2 * time.Second)
	}
}

// lines runs cmd and delivers its stdout lines.
func lines(t *testing.T, cmd *exec.Cmd) <-chan string {
	t.Helper()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ch := make(chan string, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

// next waits for a line with prefix and returns it.
func next(t *testing.T, ch <-chan string, prefix string, within time.Duration) string {
	t.Helper()
	timer := time.After(within)
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				t.Fatalf("probe ended before a %q line", prefix)
			}
			if strings.HasPrefix(l, prefix) {
				return l
			}
			if strings.HasPrefix(l, "agentprobe:") {
				t.Fatalf("probe: %s", l)
			}
		case <-timer:
			t.Fatalf("no %q line within %s", prefix, within)
		}
	}
}

// probeIn runs agentprobe in a pod's network namespace on its node.
func (o *orgEnv) probeIn(t *testing.T, nodeName, pid string, args ...string) *exec.Cmd {
	t.Helper()
	sh(t, "docker", "cp", o.probe, nodeName+":/k6-agentprobe")
	return exec.Command("docker", append([]string{"exec", nodeName, "nsenter", "-t", pid, "-n", "/k6-agentprobe"}, args...)...)
}

// --- cases ---------------------------------------------------------------------------------------

// caseOrgWalk is the §8 walk: a run whose env secret the delivery policy hands only to org_cluster
// runners (by_trust, runner_resident) lands on the org runner and its agent gets the value; the same run
// on the person's own runner (trust personal) is refused, naming the trust class.
func caseOrgWalk(t *testing.T, o *orgEnv) {
	if o.personalPool == "" {
		t.Fatal("the harness enrolled no personal runner (K6_PERSONAL_POOL empty)")
	}
	doc := map[string]any{"classes": map[string]any{}, "by_trust": map[string]any{"org_cluster": map[string]any{"classes": map[string]any{"env_secret": map[string]any{"mode": "runner_resident"}}}}}
	if code, raw := o.api(t, o.secToken, http.MethodPut, "/api/v1/governance/credential-delivery", doc, nil); code != http.StatusOK {
		t.Fatalf("PUT credential-delivery = %d %s", code, raw)
	}
	o.sched(t, o.workers(t)[len(o.workers(t))-1]) // off the worker fence-live-http2 stops
	value := "k6-resident-" + randHex()
	if code, raw := o.api(t, o.adminToken, http.MethodPut, "/api/v1/secrets/k6-op-secret", map[string]any{"value": value}, nil); code >= 300 {
		t.Fatalf("PUT secret = %d %s", code, raw)
	}
	id := o.live(t, map[string]any{"inline_policy": orgPolicy(residentGrant)})
	r := o.run(t, id)
	if r.RunnerID != o.runnerID {
		t.Fatalf("run on runner %q, want the org runner %s", r.RunnerID, o.runnerID)
	}
	got := strings.TrimSpace(o.kubectl(t, "-n", o.runsNS, "exec", "wardyn-agent-"+id, "-c", "agent", "--", "sh", "-c", "printf %s \"$RES_TOKEN\" | wc -c"))
	if got != strconv.Itoa(len(value)) {
		t.Fatalf("agent RES_TOKEN length %s, want %d", got, len(value))
	}
	keys := o.kubectl(t, "-n", o.ownNS, "get", "secrets", "-o", "name")
	if !strings.Contains(keys, "secret/wardyn-resident-") {
		t.Fatalf("no resident keystore Secret in %s: %s", o.ownNS, keys)
	}
	o.residentRun = id
	note(t, "org runner (org_cluster): run %s RUNNING, agent holds the %d-byte value, keystore Secret in %s", id, len(value), o.ownNS)

	body := map[string]any{"agent": "claude-code", "task": "sleep 3000", "placement": "local", "runner_pool_id": o.personalPool, "inline_policy": orgPolicy(residentGrant)}
	code, raw := o.api(t, o.secToken, http.MethodPost, "/api/v1/runs", body, nil)
	if code != http.StatusUnprocessableEntity || !strings.Contains(raw, "trust class personal") {
		t.Fatalf("same run on the personal runner = %d %s, want 422 naming trust class personal", code, raw)
	}
	note(t, "personal runner: %d %s", code, raw)
}

// caseOrgHardDeny: an agent's requests for in-cluster names and addresses are refused by the proxy's
// hard deny even when the run's policy allows them exactly (design §5.3: before every admission,
// the exact-literal trust included). example.com is the control that the proxy is serving.
func caseOrgHardDeny(t *testing.T, o *orgEnv) {
	rd := o.runnerdIP(t)
	apiHost, _, err := net.SplitHostPort(o.apiserver)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]string{
		"kubernetes.default.svc:443":               "service name (svc suffix)",
		"kubernetes.default.svc.cluster.local:443": "service name (cluster domain)",
		"wardyn-org." + o.orgNS + ":8080":          "a name resolving to a ClusterIP",
		net.JoinHostPort(o.relayIP, "8443"):        "relay ClusterIP literal",
		net.JoinHostPort(rd, "8443"):               "runnerd pod IP literal",
		o.apiserver:                                "apiserver address literal",
		"10.96.0.1:443":                            "service CIDR literal",
	}
	allowed := []string{"kubernetes.default.svc", "kubernetes.default.svc.cluster.local", "wardyn-org." + o.orgNS, o.relayIP, rd, apiHost, "10.96.0.1"}
	id := o.live(t, map[string]any{"inline_policy": orgPolicy(nil, allowed...)})
	deadline := time.Now().Add(90 * time.Second)
	for got := o.connect(t, id, "example.com:443"); got != "status 200"; got = o.connect(t, id, "example.com:443") {
		if time.Now().After(deadline) {
			t.Fatalf("control example.com:443 = %q, want status 200 (proxy not serving)", got)
		}
		time.Sleep(3 * time.Second)
	}
	for _, target := range slices.Sorted(maps.Keys(targets)) {
		got := o.connect(t, id, target)
		if !strings.HasPrefix(got, "status 403") {
			t.Errorf("%s (%s): %q, want a 403 refusal", target, targets[target], got)
		}
	}
	dec := o.decisions(t, id)
	for _, target := range slices.Sorted(maps.Keys(targets)) {
		host, _, _ := net.SplitHostPort(target)
		for _, d := range dec[host] {
			if !strings.HasPrefix(d, "deny:") {
				t.Errorf("%s: proxy decision %s, want only denies", host, d)
			}
		}
		if len(dec[host]) == 0 {
			t.Errorf("%s: no proxy decision logged", host)
		}
	}
	note(t, "%d in-cluster targets allowed by policy, each refused 403 and logged deny; control example.com 200", len(targets))
}

// caseOrgFence is §8 R4-M1 "Fence": a long-lived connection from the proxy's address through the relay
// (HTTP/2 when the org offers it, as the proxy's renew and activity clients use) is cut when the run is
// fenced, and a new one from that address is refused. The node's kubelet is stopped first, so the proxy
// pod lives on past its delete and only the relay's fence can cut it.
func caseOrgFence(t *testing.T, o *orgEnv) {
	ws := o.workers(t)
	o.sched(t, ws[0])
	id := o.live(t, nil)
	nodeName, pid := o.proxyNetns(t, id)
	ch := lines(t, o.probeIn(t, nodeName, pid, "h2watch", net.JoinHostPort(o.relayIP, "8443"), o.orgHost, "300"))
	proto := next(t, ch, "proto", 20*time.Second)
	for range 3 {
		next(t, ch, "ok", 20*time.Second)
	}
	kubelet(t, nodeName, false) // restarted by restart-fence-replay-reregister-sinks, or at the suite's end
	killed := time.Now()
	if code, raw := o.api(t, o.secToken, http.MethodPost, "/api/v1/runs/"+id+"/kill", nil, nil); code >= 300 {
		t.Fatalf("kill = %d %s", code, raw)
	}
	cut := next(t, ch, "cut after", 90*time.Second)
	after := time.Since(killed).Round(time.Second)
	redial := next(t, ch, "redial", 20*time.Second)
	if redial == "redial: connected" {
		t.Fatalf("a new connection from the fenced proxy's address was admitted (%s)", cut)
	}
	if phase := strings.TrimSpace(o.kubectl(t, "-n", o.runsNS, "get", "pod", "wardyn-proxy-"+id, "-o", "jsonpath={.status.phase}")); phase != "Running" {
		t.Fatalf("proxy pod phase %q after the kill, want Running (the kubelet is stopped): the cut is not the fence's", phase)
	}
	o.fenced = &fencedProxy{id: id, node: nodeName, pid: pid}
	note(t, "%s; connection cut %s after the kill (%s); %s; proxy pod still Running on %s (kubelet stopped)", proto, after, cut, redial, nodeName)
}

// caseOrgRestart is §8 R4-M1 "After a restart the fence is replayed before Online" plus the sink
// re-registration: runnerd restarts while fence-live-http2's proxy still lives; once the org sees it
// online again, a connection from the fenced address is still refused, and a run started before the
// restart on the other worker still lands its decisions at the org (its proxy's sink reconnected).
func caseOrgRestart(t *testing.T, o *orgEnv) {
	if o.fenced == nil {
		t.Fatal("fence-live-http2 left no fenced proxy")
	}
	f := o.fenced
	ws := o.workers(t)
	o.sched(t, ws[len(ws)-1]) // the boot canary and run B go to the worker whose kubelet runs
	b := o.live(t, nil)
	if got := o.connect(t, b, "example.com:443"); got != "status 200" {
		t.Fatalf("run B control before the restart: %q", got)
	}
	restarted := time.Now()
	o.kubectl(t, "-n", o.ownNS, "rollout", "restart", "deployment/"+o.deploy)
	o.kubectl(t, "-n", o.ownNS, "rollout", "status", "deployment/"+o.deploy, "--timeout=300s")
	deadline := time.Now().Add(5 * time.Minute)
	for !strings.Contains(o.kubectl(t, "-n", o.ownNS, "logs", "deployment/"+o.deploy, "-c", "runnerd"), "k8s runner ready to connect") {
		if time.Now().After(deadline) {
			t.Fatal("runnerd did not finish its boot checks within 5 min of the restart")
		}
		time.Sleep(2 * time.Second)
	}
	note(t, "runnerd relay up again %s after the restart", time.Since(restarted).Round(time.Second))

	out, _ := o.probeIn(t, f.node, f.pid, "h2watch", net.JoinHostPort(o.relayIP, "8443"), o.orgHost, "5").CombinedOutput()
	if strings.Contains(string(out), "proto") {
		t.Fatalf("after the restart a connection from the fenced proxy's address was admitted: %s", out)
	}
	note(t, "fenced address after the restart: %s", strings.TrimSpace(string(out)))

	// §8 P2: the restart reaches READY within the budget while one run's containment is pending.
	if err := o.waitOnline(3 * time.Minute); err != nil {
		t.Errorf("runner not online at the org 3 min after the restart, with run %s's containment pending on a node whose kubelet is stopped (§8 P2 wants READY within the budget): %v", f.id, err)
	}
	kubelet(t, f.node, true)
	o.kubectl(t, "-n", o.runsNS, "wait", "--for=delete", "pod/wardyn-proxy-"+f.id, "--timeout=300s")
	r := o.waitState(t, f.id, 5*time.Minute, "KILLED", "FAILED", "COMPLETED")
	o.fenced = nil
	o.online(t, 5*time.Minute)
	note(t, "kubelet back: fenced run's proxy pod deleted, run %s, runner online %s after the restart", r.State, time.Since(restarted).Round(time.Second))

	host := "k6-after-restart-" + short() + ".example.org"
	if got := o.connect(t, b, host+":443"); !strings.HasPrefix(got, "status 403") {
		t.Fatalf("run B %s: %q, want a policy refusal", host, got)
	}
	o.waitAudit(t, b, 2*time.Minute, "egress.deny for "+host, func(r auditRow) bool {
		return r.Action == "egress.deny" && strings.Contains(r.Target+string(r.Data), host)
	})
	note(t, "run B (started before the restart) decision for %s reached the org's audit", host)
}

// waitOnline is online without failing the test.
func (o *orgEnv) waitOnline(within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "https://"+o.orgHost+"/api/v1/runners/"+o.runnerID, nil)
		req.Header.Set("Authorization", "Bearer "+o.secToken)
		if resp, err := o.http.Do(req); err == nil {
			var rn struct {
				Online bool `json:"online"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&rn)
			_ = resp.Body.Close()
			if rn.Online {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("still offline after %s", within)
}

// caseOrgKeystoreRevoke is the §8 checklist item: revoking the runner deletes its keystore Secrets.
// It runs last: the org runner is revoked.
func caseOrgKeystoreRevoke(t *testing.T, o *orgEnv) {
	if o.residentRun == "" {
		t.Fatal("walk-trust-placement left no resident run")
	}
	before := o.residentSecrets(t)
	if len(before) == 0 {
		t.Fatal("no resident keystore Secret before the revoke")
	}
	if code, raw := o.api(t, o.secToken, http.MethodDelete, "/api/v1/runners/"+o.runnerID, nil, nil); code >= 300 {
		t.Fatalf("revoke = %d %s", code, raw)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for len(o.residentSecrets(t)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("keystore Secrets %v still present 3 min after the revoke", o.residentSecrets(t))
		}
		time.Sleep(2 * time.Second)
	}
	note(t, "revoke deleted %d keystore Secret(s) %v", len(before), before)
}

// lapseSetup starts self-lapse-end-to-end early, since its proof waits out one renew interval: a live
// run renews through the relay once, then its row is made terminal in the org's database (the fault: the
// org ends a run without the stop reaching runnerd), so its next renew (10 min +-1 after a success) is
// refused permanently.
func (o *orgEnv) lapseSetup(t *testing.T) {
	o.lapse = &lapseRun{}
	ok := t.Run("self-lapse-setup", func(t *testing.T) {
		o.sched(t, o.workers(t)[len(o.workers(t))-1])
		id := o.live(t, nil)
		o.lapse.id = id
		o.waitAudit(t, id, 2*time.Minute, "a successful renew", func(r auditRow) bool { return r.Action == "identity.renew" && r.Outcome == "success" })
		o.kubectl(t, "-n", o.orgNS, "exec", "deploy/postgres", "--", "psql", "-U", "wardyn", "-d", "wardyn", "-qtAc",
			"UPDATE agent_runs SET state = 'FAILED' WHERE id = '"+id+"'")
		o.lapse.marked = time.Now()
	})
	if !ok {
		o.lapse.err = "setup failed (see self-lapse-setup)"
	}
}

// caseOrgSelfLapse is §8 self-lapse "Permanent refusal" end to end: the renew is refused through the
// relay (403 run_terminal), the proxy lapses, sends its lapsed frame on runnerd's sink, and the sandbox
// is torn down.
func caseOrgSelfLapse(t *testing.T, o *orgEnv) {
	if o.lapse == nil || o.lapse.err != "" || o.lapse.id == "" {
		t.Fatalf("self-lapse setup: %+v", o.lapse)
	}
	id := o.lapse.id
	row := o.waitAudit(t, id, time.Until(o.lapse.marked.Add(14*time.Minute)), "a refused renew", func(r auditRow) bool {
		return r.Action == "identity.renew" && r.Outcome == "denied"
	})
	note(t, "renew refused at the org %s after the run was ended: %s", time.Since(o.lapse.marked).Round(time.Second), row.Data)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "logs", "wardyn-proxy-"+id, "-c", "wardyn-proxy").CombinedOutput()
		if strings.Contains(string(out), "refused permanently, lapsing") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy log shows no lapse 2 min after the refusal: %s", tail(string(out), 600))
		}
		time.Sleep(2 * time.Second)
	}
	o.kubectl(t, "-n", o.runsNS, "wait", "--for=delete", "pod/wardyn-agent-"+id, "pod/wardyn-proxy-"+id, "--timeout=300s")
	note(t, "proxy logged the permanent refusal and lapsed; runnerd tore the sandbox down %s after the mark", time.Since(o.lapse.marked).Round(time.Second))
}

// caseOrgSweepRollback: a run whose sandbox creation the org gave up on (runnerd went away mid-create)
// must not leave its pods on the runner once runnerd is back; the org has to roll them back.
func caseOrgSweepRollback(t *testing.T, o *orgEnv) {
	ws := o.workers(t)
	o.sched(t, ws[len(ws)-1])
	id := o.launch(t, "sleep 3000", nil)
	t.Cleanup(func() {
		_, _ = exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "delete", "pod", "wardyn-agent-"+id, "wardyn-proxy-"+id, "--ignore-not-found", "--wait=false").CombinedOutput()
	})
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "get", "pod", "wardyn-proxy-"+id).Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no proxy pod within 3 min")
		}
		time.Sleep(200 * time.Millisecond)
	}
	o.kubectl(t, "-n", o.ownNS, "scale", "deployment/"+o.deploy, "--replicas=0")
	o.kubectl(t, "-n", o.ownNS, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/instance=k6,app.kubernetes.io/component=runnerd", "--timeout=180s")
	r := o.waitState(t, id, 3*time.Minute, "FAILED", "KILLED")
	if err := exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "get", "pod", "wardyn-proxy-"+id).Run(); err != nil {
		o.kubectl(t, "-n", o.ownNS, "scale", "deployment/"+o.deploy, "--replicas=1")
		o.online(t, 5*time.Minute)
		inconclusive(t, "runnerd removed the sandbox before it stopped; no leftover to roll back")
	}
	o.kubectl(t, "-n", o.ownNS, "scale", "deployment/"+o.deploy, "--replicas=1")
	o.kubectl(t, "-n", o.ownNS, "rollout", "status", "deployment/"+o.deploy, "--timeout=300s")
	o.online(t, 5*time.Minute)
	back := time.Now()
	deadline = time.Now().Add(5 * time.Minute)
	for exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", o.runsNS, "get", "pod", "wardyn-proxy-"+id).Run() == nil {
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %s at the org, but its proxy pod is still on the runner 5 min after runnerd came back: nothing rolled it back", id, r.State)
		}
		time.Sleep(2 * time.Second)
	}
	note(t, "run %s %s at the org; its leftover pods were removed %s after runnerd came back", id, r.State, time.Since(back).Round(time.Second))
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (o *orgEnv) residentSecrets(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range strings.Fields(o.kubectl(t, "-n", o.ownNS, "get", "secrets", "-o", "name")) {
		if strings.HasPrefix(n, "secret/wardyn-resident-") {
			out = append(out, n)
		}
	}
	return out
}

func TestK6Org(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K6") != "1" {
		t.Skip("WARDYN_TEST_K6=1 not set; run scripts/k6-kind-conformance.sh")
	}
	o := newOrgEnv(t)
	var results []string
	kase := func(name string, fn func(t *testing.T, o *orgEnv)) {
		var status, detail string
		t.Run(name, func(t *testing.T) {
			defer func() {
				detail = notes(t)
				switch {
				case t.Failed():
					status = "FAIL"
				case t.Skipped():
					status = skipStatus(t)
				default:
					status = "PASS"
				}
			}()
			fn(t, o)
		})
		results = append(results, fmt.Sprintf("K6-CASE\t%s\t%s\t%s", name, status, detail))
	}
	t.Cleanup(func() {
		for _, n := range o.workers(t) {
			_ = exec.Command("docker", "exec", n, "systemctl", "start", "kubelet").Run()
		}
	})
	o.lapseSetup(t)
	kase("walk-trust-placement", caseOrgWalk)
	kase("hard-deny-from-agent", caseOrgHardDeny)
	kase("fence-live-http2", caseOrgFence)
	kase("restart-fence-replay-reregister-sinks", caseOrgRestart)
	kase("self-lapse-end-to-end", caseOrgSelfLapse)
	kase("org-triggered-sweep-rollback", caseOrgSweepRollback)
	kase("keystore-deleted-on-revoke", caseOrgKeystoreRevoke) // last: revokes the org runner
	// No MITM host can reach an org runner's proxy in this build: the org refuses a run whose
	// injected credential would need runner_resident delivery ("not built yet"), and a local plan
	// strips every other MITM host. The drain half is pinned in process instead.
	results = append(results, "K6-CASE\tmitm-inner-request-drain\tBLOCKED\tno MITM host reaches an org runner's proxy (runner_resident delivery of an injected credential is not built); "+
		"pinned in process by internal/egress/proxy TestRemoteSink_DrainEndsInFlightMITMRequest")
	for _, r := range results {
		fmt.Println(r)
	}
}
