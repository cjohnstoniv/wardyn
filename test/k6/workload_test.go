// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

// TestK6Workload is the declarative-workload path end to end on a real cluster (#3804), run by
// scripts/k6-kind-conformance.sh with K6_SUITES=workload. Two people sign in through a throwaway Dex in
// the cluster: a security operator binds a runs namespace on the org pool to a service identity the
// other person sponsors. A WardynWorkload applied in that namespace is reported by runnerd, launched by
// the org as a run on the bound pool through the org runner, and its status is written back with the
// run's id. Deleting it ends the run and the workload row. One applied in a namespace nothing binds is
// refused on its status, and nothing is recorded or launched for it.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// dexPassword is the public demo password of deploy/kind/sso/dex.yaml's bcrypt hash (the same literal
// test/e2e/e2e.sh signs in with), for a throwaway Dex that lives only inside the kind cluster.
const dexPassword = "password"

// notBoundMsg is the org's answer to a resource in a namespace no binding names (runner_workloads.go).
const notBoundMsg = "No Wardyn workload binding names this namespace on this runner's pools. A security admin binds it to a service identity."

var formAction = regexp.MustCompile(`<form[^>]*action="([^"]*)"`)

// dexSession signs email in through the org's console login and Dex's password form, as a browser does,
// and returns a client holding the resulting console session. The dialer routes the org's and Dex's
// cluster names to their node ports on the kind network; each TLS dial verifies its own host.
func dexSession(t *testing.T, o *orgEnv, email string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(must(t, "K6_ORG_CA"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	addrs := map[string]string{o.orgHost: must(t, "K6_ORG_ADDR"), must(t, "K6_DEX_HOST"): must(t, "K6_DEX_ADDR")}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 60 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			to, ok := addrs[host]
			if !ok {
				return nil, fmt.Errorf("no route for %s", addr)
			}
			return dialer.DialContext(ctx, network, to)
		},
	}}
	resp, err := c.Get(o.orgURL + "/auth/login")
	if err != nil {
		t.Fatalf("sign-in %s: login: %v", email, err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := formAction.FindSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("sign-in %s: login page %s = %d, no password form: %s", email, resp.Request.URL, resp.StatusCode, tail(string(page), 200))
	}
	action, err := resp.Request.URL.Parse(html.UnescapeString(string(m[1])))
	if err != nil {
		t.Fatalf("sign-in %s: form action %q: %v", email, m[1], err)
	}
	resp, err = c.PostForm(action.String(), url.Values{"login": {email}, "password": {dexPassword}})
	if err != nil {
		t.Fatalf("sign-in %s: password form: %v", email, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var who struct {
		Method string `json:"method"`
	}
	if code, raw := sessAPI(t, o, c, http.MethodGet, "/api/v1/me", nil, &who); code != http.StatusOK || who.Method != "sso" {
		t.Fatalf("sign-in %s: ended at %s (%d: %s); GET /me = %d %s, want 200 method sso", email, resp.Request.URL.Path, resp.StatusCode, tail(string(body), 200), code, tail(raw, 200))
	}
	return c
}

// sessAPI is o.api on a console session instead of a bearer token. It sends no Origin header, as curl
// and the CLI do not.
func sessAPI(t *testing.T, o *orgEnv, c *http.Client, method, path string, body, out any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "https://"+o.orgHost+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
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

// me is GET /api/v1/me on a session.
func me(t *testing.T, o *orgEnv, c *http.Client) (principal string, securityOperator bool) {
	t.Helper()
	var m struct {
		Principal        string `json:"principal"`
		SecurityOperator bool   `json:"security_operator"`
	}
	if code, raw := sessAPI(t, o, c, http.MethodGet, "/api/v1/me", nil, &m); code != http.StatusOK || m.Principal == "" {
		t.Fatalf("GET /me = %d %s", code, raw)
	}
	return m.Principal, m.SecurityOperator
}

// applyWorkload applies a WardynWorkload named name in ns whose intent is a long sleep under the org
// cases' policy, and deletes it when t ends. A workload's inline_policy is an overlay on the deployment
// default, whose github_token grant a local runner refuses (placement_credential), so the intent names
// an empty eligible_grants to drop it.
func applyWorkload(t *testing.T, o *orgEnv, ns, name string) {
	t.Helper()
	doc := map[string]any{
		"apiVersion": "wardyn.dev/v1alpha1", "kind": "WardynWorkload",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{"intent": map[string]any{
			"agent": "claude-code", "task": "sleep 3000", "placement": "local", "inline_policy": orgPolicy([]map[string]any{})}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	o.kubectl(t, "apply", "-f", path)
	t.Cleanup(func() {
		_, _ = exec.Command("kubectl", "--kubeconfig", o.adminKC, "-n", ns, "delete", "wardynworkloads.wardyn.dev", name, "--ignore-not-found", "--wait=false").CombinedOutput()
	})
}

// workloadStatus is the resource's status field at jsonpath.
func (o *orgEnv) workloadStatus(t *testing.T, ns, name, field string) string {
	t.Helper()
	return strings.TrimSpace(o.kubectl(t, "-n", ns, "get", "wardynworkloads.wardyn.dev", name, "-o", "jsonpath={.status."+field+"}"))
}

// waitFor polls cond every 2 s until it holds, failing with what() after within.
func waitFor(t *testing.T, within time.Duration, what func() string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", within, what())
		}
		time.Sleep(2 * time.Second)
	}
}

// caseWorkload: bound launch, status, delete, and the unbound refusal (the six steps of #3804).
func caseWorkload(t *testing.T, o *orgEnv) {
	wlNS, freeNS := must(t, "K6_WL_NS"), must(t, "K6_WL_FREE_NS")

	// 1. The sponsor signs in first: the sign-in writes the identity row a service identity's sponsor
	// must hold. The security operator then makes the identity and binds the namespace on the org pool.
	sp := dexSession(t, o, must(t, "K6_WL_SPONSOR_EMAIL"))
	sponsor, _ := me(t, o, sp)
	sec := dexSession(t, o, must(t, "K6_WL_SECOP_EMAIL"))
	if _, secOp := me(t, o, sec); !secOp {
		t.Fatal("the security operator's session is not on the security tier (security_operator false)")
	}
	svc := "k6-wl-" + short()
	if code, raw := sessAPI(t, o, sec, http.MethodPost, "/api/v1/service-identities",
		map[string]any{"name": svc, "description": "k6 workload case", "sponsor": sponsor}, nil); code != http.StatusCreated {
		t.Fatalf("POST /service-identities (sponsor %s) = %d %s, want 201", sponsor, code, raw)
	}
	t.Cleanup(func() { sessAPI(t, o, sec, http.MethodDelete, "/api/v1/service-identities/"+svc, nil, nil) })
	pool := o.pool(t)
	var b struct {
		ID string `json:"id"`
	}
	if code, raw := sessAPI(t, o, sec, http.MethodPost, "/api/v1/workload-bindings",
		map[string]any{"pool": pool, "namespace": wlNS, "service": svc}, &b); code != http.StatusCreated {
		t.Fatalf("POST /workload-bindings = %d %s, want 201", code, raw)
	}
	t.Cleanup(func() { sessAPI(t, o, sec, http.MethodDelete, "/api/v1/workload-bindings/"+b.ID, nil, nil) })
	note(t, "service %s sponsored by %s, %s bound on pool %s", svc, sponsor, wlNS, pool)

	// 2. A resource in the bound namespace.
	const name = "k6-wl-run"
	applyWorkload(t, o, wlNS, name)
	applied := time.Now()

	// 3. runnerd reports it, the org launches it on the bound pool, and the run reaches RUNNING.
	rowPath := "/api/v1/workloads/" + name + "?owner=" + url.QueryEscape("service:"+svc)
	var row struct {
		State       string          `json:"state"`
		RunID       string          `json:"run_id"`
		LastRefusal json.RawMessage `json:"last_refusal"`
	}
	var rowRaw string
	waitFor(t, 4*time.Minute, func() string { return "no run for " + name + ": " + rowRaw }, func() bool {
		row.RunID = ""
		_, rowRaw = sessAPI(t, o, sec, http.MethodGet, rowPath, nil, &row)
		return row.RunID != ""
	})
	id := row.RunID
	o.runs.add(id)
	t.Cleanup(func() { o.api(t, o.secToken, http.MethodPost, "/api/v1/runs/"+id+"/kill", nil, nil) })
	o.waitState(t, id, 4*time.Minute, "RUNNING")
	running := time.Since(applied).Round(time.Second)
	var run struct {
		RunnerID     string `json:"runner_id"`
		RunnerPoolID string `json:"runner_pool_id"`
	}
	if code, raw := o.api(t, o.secToken, http.MethodGet, "/api/v1/runs/"+id, nil, &run); code != http.StatusOK {
		t.Fatalf("GET run = %d %s", code, raw)
	}
	if run.RunnerPoolID != pool || run.RunnerID != o.runnerID {
		t.Fatalf("run %s on pool %q runner %q, want the bound pool %s and the org runner %s", id, run.RunnerPoolID, run.RunnerID, pool, o.runnerID)
	}
	note(t, "run %s RUNNING on the bound pool %s %s after the apply", id, pool, running)

	// 4. The status carries the running phase and that run's id.
	var got string
	waitFor(t, 3*time.Minute, func() string { return "status " + got + ", want Running/" + id }, func() bool {
		got = o.workloadStatus(t, wlNS, name, "phase") + "/" + o.workloadStatus(t, wlNS, name, "runID")
		return got == "Running/"+id
	})
	note(t, "status Running with run %s %s after the apply", id, time.Since(applied).Round(time.Second))

	// 5. Deleting the resource ends the run, and the workload row goes.
	o.kubectl(t, "-n", wlNS, "delete", "wardynworkloads.wardyn.dev", name, "--wait=true", "--timeout=60s")
	r := o.waitState(t, id, 3*time.Minute, "KILLED") // the delete's kill, not an end of its own
	var code int
	var raw string
	waitFor(t, 3*time.Minute, func() string { return fmt.Sprintf("workload row still there: %d %s", code, raw) }, func() bool {
		code, raw = sessAPI(t, o, sec, http.MethodGet, rowPath, nil, nil)
		return code == http.StatusNotFound
	})
	note(t, "delete: run %s, workload row gone", r.State)

	// 6. A resource in a namespace nothing binds is refused on its status; nothing is recorded or launched.
	const free = "k6-wl-free"
	applyWorkload(t, o, freeNS, free)
	var phase, msg string
	waitFor(t, 2*time.Minute, func() string { return fmt.Sprintf("status %q %q, want NotBound %q", phase, msg, notBoundMsg) }, func() bool {
		phase, msg = o.workloadStatus(t, freeNS, free, "phase"), o.workloadStatus(t, freeNS, free, "message")
		return phase == "NotBound" && msg == notBoundMsg
	})
	if rid := o.workloadStatus(t, freeNS, free, "runID"); rid != "" {
		t.Fatalf("unbound resource's status names run %s", rid)
	}
	freePath := "/api/v1/workloads/" + free + "?owner=" + url.QueryEscape("service:"+svc)
	if code, raw := sessAPI(t, o, sec, http.MethodGet, freePath, nil, nil); code != http.StatusNotFound {
		t.Fatalf("GET %s = %d %s, want 404: nothing recorded for an unbound namespace", freePath, code, raw)
	}
	note(t, "%s/%s: NotBound with the org's message, no run, no workload row", freeNS, free)
}

func TestK6Workload(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K6") != "1" {
		t.Skip("WARDYN_TEST_K6=1 not set; run scripts/k6-kind-conformance.sh with K6_SUITES=workload")
	}
	o := newOrgEnv(t)
	const name = "workload-bound-launch-and-unbound-refusal"
	var status, detail string
	t.Run(name, func(t *testing.T) {
		defer func() {
			if ids := o.runs.take(); t.Failed() {
				o.dumpFailure(t, ids)
			}
		}()
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
		caseWorkload(t, o)
	})
	fmt.Printf("K6-CASE\t%s\t%s\t%s\n", name, status, detail)
}
