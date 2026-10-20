// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The kubectl row: the switch, the token route, dispatch and the sink, against
// an httptest API server. Nothing here reaches a real cluster.

const (
	kubeRegistered = "registered-sa-token-of-the-organisation"
	kubeMinted     = "minted-ten-minute-token"
	kubeOwner      = "person-sub"
)

func kubectlRowDef(server, caPEM string, namespaces ...string) types.ComponentDefinition {
	return types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindKubectl, Kubectl: &types.CLIKubectl{
		Server: server, CAPEM: caPEM, ServiceAccount: types.CLIServiceAccount{Namespace: "wardyn", Name: "runs"},
		Namespaces: namespaces, Access: types.ComponentAccessRead,
	}}}
}

// kubeAPIServer is a fake API server's TokenRequest endpoint: it records each
// request and answers status (201 by default) with a ten-minute token.
type kubeAPIServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []string // "METHOD path authorization body"
	status int
}

func newKubeAPIServer(t *testing.T) *kubeAPIServer {
	t.Helper()
	k := &kubeAPIServer{status: http.StatusCreated}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		k.mu.Lock()
		k.seen = append(k.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		status := k.status
		k.mu.Unlock()
		w.WriteHeader(status)
		if status != http.StatusCreated {
			_, _ = io.WriteString(w, `{"kind":"Status","message":"serviceaccounts \"runs\" is forbidden"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
			"token": kubeMinted, "expirationTimestamp": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		}})
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *kubeAPIServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: k.srv.Certificate().Raw}))
}

type kubeFixture struct {
	srv   *Server
	st    *azCLIStore
	sec   *memSecrets
	audit *memAudit
	id    uuid.UUID
	api   *kubeAPIServer
}

func newKubeFixture(t *testing.T) *kubeFixture {
	t.Helper()
	t.Setenv(types.EnvKubectlCLI, "on")
	f := &kubeFixture{sec: &memSecrets{m: map[string][]byte{}}, audit: &memAudit{}, id: uuid.New(), api: newKubeAPIServer(t)}
	f.st = &azCLIStore{azA3Store: &azA3Store{azureSiteStore: &azureSiteStore{}, approvals: newFakeApprovals()}}
	f.srv = &Server{cfg: Config{Store: f.st, Secrets: f.sec, MaskRegistry: secretmask.NewRegistry(), Now: time.Now, Audit: f.audit}}
	def := kubectlRowDef(f.api.srv.URL, f.api.caPEM(), "team-a", "team-b")
	f.setRow(t, def)
	f.st.run = types.AgentRun{ID: uuid.New(), CreatedBy: kubeOwner}
	f.st.attached = []types.RunComponent{{ComponentID: &f.id, Name: "Cluster", Definition: def}}
	return f
}

func (f *kubeFixture) setRow(t *testing.T, def types.ComponentDefinition) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.GetComponent(ctx, f.id, ""); err == nil {
		if _, err := f.st.DeleteComponent(ctx, f.id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.CreateComponent(ctx, types.Component{ID: f.id, Name: "Cluster", Definition: def}); err != nil {
		t.Fatal(err)
	}
}

func (f *kubeFixture) dispatch(caCert string) ([]runner.InjectionGrant, kubectlCLIPlan, map[string]string, *types.RunPolicySpec, bool) {
	env, policy := map[string]string{}, &types.RunPolicySpec{}
	inj, plan, ok := f.srv.authorKubectlCLI(context.Background(), f.st.run,
		runComponents{attached: []attachedComponent{{snapshot: f.st.attached[0]}}}.kubectlCLI(), caCert, "ca-key", policy, env, nil)
	return inj, plan, env, policy, ok
}

func (f *kubeFixture) resolve(rule egress.InjectionRule, grantID uuid.UUID, sub string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/injection/"+grantID.String(), nil)
	if !f.srv.resolveKubectlCLIInjection(w, r, &identity.Claims{RunID: f.st.run.ID, Sub: sub, SPIFFEID: "spiffe://wardyn.local/run"},
		broker.Minted{JTI: "jti-" + uuid.NewString(), Injection: &rule}, grantID) {
		w.WriteHeader(http.StatusTeapot) // the arm stood aside
	}
	return w
}

func (f *kubeFixture) putToken(id uuid.UUID, body string) *httptest.ResponseRecorder {
	rt := chi.NewRouter()
	rt.Put("/components/{id}/kubectl-token", f.srv.handlePutKubectlToken)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/components/"+id.String()+"/kubectl-token", strings.NewReader(body)))
	return w
}

// ── off is today ────────────────────────────────────────────────────────────

// TestKubectlCLI_OffIsToday is the default-off golden: with WARDYN_KUBECTL_CLI
// unset, every surface answers exactly as it did before the kind existed.
func TestKubectlCLI_OffIsToday(t *testing.T) {
	f := newKubeFixture(t)
	t.Setenv(types.EnvKubectlCLI, "")
	row := types.Component{ID: f.id, Name: "Cluster", Definition: kubectlRowDef("https://k8s.example.com", "", "team-a")}

	// The catalogue: the five cards as before, kubectl coming_soon even with a granted row.
	const today = `[{"kind":"azure","state":"not_granted","rows":[]},{"kind":"github","state":"not_granted","rows":[]},` +
		`{"kind":"aws","state":"not_granted","rows":[]},{"kind":"gcloud","state":"coming_soon","rows":[]},{"kind":"kubectl","state":"coming_soon","rows":[]}]`
	for _, granted := range [][]client.OrgComponentView{nil, {orgComponentView(row)}} {
		if got := string(mustJSON(catalogueCLIs(granted))); got != today {
			t.Errorf("catalogue = %s\nwant %s", got, today)
		}
	}
	// A save: today's unknown-kind refusal, byte for byte.
	if reason, msg := f.srv.componentSaveRefusal(context.Background(), row); reason != reasonComponentDefinitionInvalid ||
		msg != `invalid component: definition.cli.kind: "kubectl" is not azure, aws or github` {
		t.Errorf("save = %q %q, want today's unknown-kind refusal", reason, msg)
	}
	// The gate on a stored row: the same problem.
	if p := componentDefinitionProblem(attachedComponent{snapshot: types.RunComponent{ComponentID: &f.id, Definition: row.Definition}}); p != `cli.kind: "kubectl" is not azure, aws or github` {
		t.Errorf("gate problem = %q", p)
	}
	// The token route is not there: the router's own 404.
	w := f.putToken(f.id, `{"value":"`+kubeRegistered+`"}`)
	if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
		t.Errorf("token route = %d %q, want the router's 404", w.Code, w.Body.String())
	}
	if len(f.sec.m) != 0 {
		t.Errorf("the off route stored %v", f.sec.m)
	}
}

// ── on ──────────────────────────────────────────────────────────────────────

func TestKubectlCLI_CatalogueAndView(t *testing.T) {
	f := newKubeFixture(t)
	cards := catalogueCLIs(nil)
	if cards[4].Kind != "kubectl" || cards[4].State != client.CLIStateNotGranted {
		t.Fatalf("kubectl card = %+v, want not_granted", cards[4])
	}
	row := types.Component{ID: f.id, Name: "Cluster", Definition: kubectlRowDef("https://K8S.example.com:6443", "", "team-a")}
	v := orgComponentView(row)
	cards = catalogueCLIs([]client.OrgComponentView{v})
	if cards[4].State != client.CLIStateAvailable || len(cards[4].Rows) != 1 {
		t.Fatalf("kubectl card = %+v, want available with the row", cards[4])
	}
	if v.CLI.Cluster != "k8s.example.com" || len(v.CLI.Namespaces) != 1 || v.CLI.Access != "read" {
		t.Errorf("view = %+v", v.CLI)
	}
	if raw := string(mustJSON(v)); strings.Contains(raw, "service_account") || strings.Contains(raw, "ca_pem") {
		t.Errorf("the member's view carries the account or the CA: %s", raw)
	}
}

func TestKubectlCLI_TokenRoute(t *testing.T) {
	f := newKubeFixture(t)
	name := kubectlTokenSecretName(f.id.String())
	if w := f.putToken(f.id, `{"value":"`+kubeRegistered+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	if got := string(f.sec.m[name]); got != kubeRegistered {
		t.Fatalf("operator namespace holds %q under %s", got, name)
	}
	if rows := f.audit.find("secret.write"); len(rows) != 1 || rows[0].Target != name || rows[0].Outcome != "success" ||
		strings.Contains(string(rows[0].Data), kubeRegistered) {
		t.Errorf("audit = %+v", rows)
	}
	for body, want := range map[string]int{`{"value":""}`: 400, `{"value":"a b c d e f g h"}`: 400, `nope`: 400, `{"value":"short"}`: 400} {
		if w := f.putToken(f.id, body); w.Code != want {
			t.Errorf("body %s = %d, want %d", body, w.Code, want)
		}
	}
	if w := f.putToken(uuid.New(), `{"value":"`+kubeRegistered+`"}`); w.Code != http.StatusNotFound {
		t.Errorf("no row = %d", w.Code)
	}
	f.setRow(t, azCLIRowDef("read", azCLISubA))
	if w := f.putToken(f.id, `{"value":"`+kubeRegistered+`"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an Azure CLI row = %d", w.Code)
	}
	// The secrets API refuses the name, and the row's purge takes it.
	if !secretsAPIReserved(name) || !sinkReservedSecret(name) {
		t.Errorf("%s is not reserved", name)
	}
	if err := f.srv.purgeCLISignIns(context.Background(), f.id.String()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.sec.m[name]; ok {
		t.Error("the purge left the registered token")
	}
}

func TestKubectlCLIPinChanged(t *testing.T) {
	base := kubectlRowDef("https://a.example", "", "x")
	moved := func(f func(*types.CLIKubectl)) types.ComponentDefinition {
		d := kubectlRowDef("https://a.example", "", "x")
		f(d.CLI.Kubectl)
		return d
	}
	for name, c := range map[string]struct {
		after types.ComponentDefinition
		want  bool
	}{
		"same":            {kubectlRowDef("https://a.example", "", "x"), false},
		"namespaces only": {moved(func(k *types.CLIKubectl) { k.Namespaces = []string{"y"} }), false},
		"server":          {moved(func(k *types.CLIKubectl) { k.Server = "https://b.example" }), true},
		"ca":              {moved(func(k *types.CLIKubectl) { k.CAPEM = "x" }), true},
		"account":         {moved(func(k *types.CLIKubectl) { k.ServiceAccount.Name = "other" }), true},
		"not kubectl":     {azCLIRowDef("read", azCLISubA), true},
	} {
		if got := kubectlCLIPinChanged(base, c.after); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestKubectlCLI_ComponentGate(t *testing.T) {
	f := newKubeFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/runs", nil)
	att := attachedComponent{snapshot: f.st.attached[0]}
	adm := &componentAdmission{credentials: true}
	if refused := f.srv.componentKubectlCLIAdmitted(r, adm, &att, 0); refused == nil || refused.reason != reasonComponentSecretMissing {
		t.Fatalf("unregistered = %+v, want secret missing", refused)
	}
	f.sec.m[kubectlTokenSecretName(f.id.String())] = []byte(kubeRegistered)
	adm = &componentAdmission{credentials: true}
	if refused := f.srv.componentKubectlCLIAdmitted(r, adm, &att, 0); refused != nil {
		t.Fatalf("registered = %+v", refused)
	}
	if refused := f.srv.componentKubectlCLIAdmitted(r, adm, &att, 1); refused == nil || !strings.Contains(refused.refusal.body.Error, "second kubectl") {
		t.Fatalf("second row = %+v", refused)
	}
	// The preview records the missing token instead of refusing.
	delete(f.sec.m, kubectlTokenSecretName(f.id.String()))
	att = attachedComponent{snapshot: f.st.attached[0]}
	if refused := f.srv.componentKubectlCLIAdmitted(r, &componentAdmission{}, &att, 0); refused != nil || !att.needsAdminSecret {
		t.Fatalf("preview = %+v needsAdminSecret=%v", refused, att.needsAdminSecret)
	}
}

// Dispatch stages a kubeconfig with the run's CA and an inert token, opens the
// server's port, and writes a grant that names the reserved token and the
// snapshot. The registered token is nowhere in what the sandbox gets.
func TestKubectlCLI_DispatchStagesConfigNotToken(t *testing.T) {
	f := newKubeFixture(t)
	f.sec.m[kubectlTokenSecretName(f.id.String())] = []byte(kubeRegistered)
	inj, plan, env, policy, ok := f.dispatch("run-ca-cert")
	if !ok || len(inj) != 1 {
		t.Fatalf("dispatch ok=%v injections=%d", ok, len(inj))
	}
	host, port, _ := types.CLIKubectl{Server: f.api.srv.URL}.HostPort()
	if inj[0].Rule.Host != host || !inj[0].Rule.RequireTLS || inj[0].Rule.SecretName != kubectlTokenSecretName(f.id.String()) {
		t.Errorf("rule = %+v", inj[0].Rule)
	}
	if g := plan.gate; g == nil || g.Host != host || g.Port != port || g.Write || len(g.Namespaces) != 2 || g.CAPEM != f.api.caPEM() {
		t.Errorf("door = %+v", plan.gate)
	}
	if want := host + ":" + strconv.Itoa(port); len(policy.AllowedDomains) != 1 || policy.AllowedDomains[0] != want {
		t.Errorf("allowlist = %v, want %s", policy.AllowedDomains, want)
	}
	for k, v := range env {
		if strings.Contains(v, kubeRegistered) {
			t.Errorf("sandbox env %s carries the registered token", k)
		}
	}
	line := env[kubectlCLIConfigEnvVar]
	path, b64, _ := strings.Cut(line, "\t")
	raw, _ := base64.StdEncoding.DecodeString(b64)
	var cfg struct {
		Clusters []struct {
			Cluster map[string]string `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User map[string]string `json:"user"`
		} `json:"users"`
		Contexts []struct {
			Context map[string]string `json:"context"`
		} `json:"contexts"`
	}
	if path != ".kube/config" || json.Unmarshal(raw, &cfg) != nil || len(cfg.Clusters) != 1 || len(cfg.Users) != 1 {
		t.Fatalf("staged %q: %s", path, raw)
	}
	ca, _ := base64.StdEncoding.DecodeString(cfg.Clusters[0].Cluster["certificate-authority-data"])
	if cfg.Clusters[0].Cluster["server"] != f.api.srv.URL || string(ca) != "run-ca-cert" ||
		cfg.Users[0].User["token"] != kubectlCLITokenSentinel || cfg.Contexts[0].Context["namespace"] != "team-a" {
		t.Errorf("kubeconfig = %s", raw)
	}
	var sc struct {
		Snapshot kubectlGrantSnapshot `json:"snapshot"`
	}
	if len(f.st.grants) != 1 || json.Unmarshal(f.st.grants[0].Spec.Scope, &sc) != nil || !sc.Snapshot.authored() || sc.Snapshot.OwnerSubject != kubeOwner {
		t.Errorf("grant = %+v", f.st.grants)
	}
}

func TestKubectlCLI_DispatchRefusesWithoutRunCA(t *testing.T) {
	f := newKubeFixture(t)
	if inj, plan, env, _, ok := f.dispatch(""); ok || len(inj) != 0 || plan.gate != nil || len(env) != 0 {
		t.Fatalf("dispatch without a CA: ok=%v inj=%d gate=%v env=%v", ok, len(inj), plan.gate, env)
	}
	if f.st.failedTo != types.RunFailed || f.st.hint != kubectlCLINoCertificateAuthority {
		t.Errorf("run failed=%v hint=%q", f.st.failedTo, f.st.hint)
	}
}

// The ceiling's withholding of the credential takes the door and the entry
// the lane opened with it.
func TestKubectlCLIPlanCloseWithheld(t *testing.T) {
	f := newKubeFixture(t)
	inj, plan, _, policy, _ := f.dispatch("run-ca-cert")
	if got := plan.closeWithheld(policy, inj); got != plan.gate {
		t.Fatalf("kept credential: door = %v", got)
	}
	if got := plan.closeWithheld(policy, nil); got != nil || len(policy.AllowedDomains) != 0 {
		t.Fatalf("withheld: door = %v allowlist = %v", got, policy.AllowedDomains)
	}
}

// The resolve trades the registered token for a ten-minute one at the row's
// API server, trusting the row's CA, and hands only the minted token on.
func TestKubectlCLI_ResolveMintsShortLived(t *testing.T) {
	f := newKubeFixture(t)
	f.sec.m[kubectlTokenSecretName(f.id.String())] = []byte(kubeRegistered)
	inj, _, _, _, _ := f.dispatch("run-ca-cert")
	w := f.resolve(inj[0].Rule, inj[0].GrantID, kubeOwner)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	var got injectionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Value != "Bearer "+kubeMinted || got.Header != "Authorization" || strings.Contains(w.Body.String(), kubeRegistered) {
		t.Errorf("response = %s", w.Body)
	}
	if exp := time.UnixMilli(got.ExpiresAt); exp.After(time.Now().Add(11 * time.Minute)) {
		t.Errorf("expires %v, beyond the minted token", exp)
	}
	f.api.mu.Lock()
	seen := append([]string(nil), f.api.seen...)
	f.api.mu.Unlock()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "POST /api/v1/namespaces/wardyn/serviceaccounts/runs/token Bearer "+kubeRegistered+" ") ||
		!strings.Contains(seen[0], `"expirationSeconds":600`) {
		t.Errorf("the API server saw %q", seen)
	}
	rows := f.audit.find("secret.read")
	if len(rows) != 1 || rows[0].Outcome != "success" || strings.Contains(string(rows[0].Data), kubeMinted) || strings.Contains(string(rows[0].Data), kubeRegistered) {
		t.Errorf("audit = %+v", rows)
	}
}

func TestKubectlCLI_ResolveRefusals(t *testing.T) {
	type tc struct {
		name   string
		mutate func(*testing.T, *kubeFixture, *egress.InjectionRule) string // returns the subject to resolve as
		status int
		reason string
	}
	for _, c := range []tc{
		{"another subject", func(*testing.T, *kubeFixture, *egress.InjectionRule) string { return "someone-else" }, http.StatusForbidden, reasonOwnerNotCaller},
		{"another host", func(_ *testing.T, _ *kubeFixture, r *egress.InjectionRule) string {
			r.Host = "evil.example"
			return kubeOwner
		}, http.StatusForbidden, reasonHostNotEndpoint},
		{"switched off", func(t *testing.T, _ *kubeFixture, _ *egress.InjectionRule) string {
			t.Setenv(types.EnvKubectlCLI, "off")
			return kubeOwner
		}, http.StatusForbidden, reasonScopeChanged},
		{"namespaces changed", func(t *testing.T, f *kubeFixture, _ *egress.InjectionRule) string {
			f.setRow(t, kubectlRowDef(f.api.srv.URL, f.api.caPEM(), "team-a", "kube-system"))
			return kubeOwner
		}, http.StatusForbidden, reasonScopeChanged},
		{"row gone from the run", func(_ *testing.T, f *kubeFixture, _ *egress.InjectionRule) string {
			f.st.attached = nil
			return kubeOwner
		}, http.StatusForbidden, reasonScopeChanged},
		{"unregistered", func(_ *testing.T, f *kubeFixture, _ *egress.InjectionRule) string {
			delete(f.sec.m, kubectlTokenSecretName(f.id.String()))
			return kubeOwner
		}, http.StatusFailedDependency, reasonSinkSecretNotFound},
		{"the cluster refuses", func(_ *testing.T, f *kubeFixture, _ *egress.InjectionRule) string {
			f.api.status = http.StatusForbidden
			return kubeOwner
		}, http.StatusBadGateway, reasonSinkResolveFailed},
		{"the CA changed", func(t *testing.T, f *kubeFixture, _ *egress.InjectionRule) string {
			f.setRow(t, kubectlRowDef(f.api.srv.URL, "", "team-a", "team-b"))
			return kubeOwner
		}, http.StatusForbidden, reasonScopeChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newKubeFixture(t)
			f.sec.m[kubectlTokenSecretName(f.id.String())] = []byte(kubeRegistered)
			inj, _, _, _, _ := f.dispatch("run-ca-cert")
			rule := inj[0].Rule
			sub := c.mutate(t, f, &rule)
			w := f.resolve(rule, inj[0].GrantID, sub)
			if w.Code != c.status || !strings.Contains(w.Body.String(), `"`+c.reason+`"`) {
				t.Fatalf("resolve = %d %s, want %d %s", w.Code, w.Body, c.status, c.reason)
			}
			if strings.Contains(w.Body.String(), kubeRegistered) || strings.Contains(w.Body.String(), kubeMinted) {
				t.Errorf("a refusal carries a token: %s", w.Body)
			}
		})
	}
}

// The resolve stands aside for every name that is not a kubectl row's token.
func TestKubectlCLI_ResolveStandsAside(t *testing.T) {
	f := newKubeFixture(t)
	for _, name := range []string{"wardyn-cli-" + f.id.String() + "-entra", "my-key", "wardyn-cli--kubectl"} {
		if w := f.resolve(egress.InjectionRule{Host: "x", SecretName: name}, uuid.New(), kubeOwner); w.Code != http.StatusTeapot {
			t.Errorf("%s: handled with %d", name, w.Code)
		}
	}
}

// The mint trusts the row's CA bundle and nothing else: without it, the API
// server's certificate is not trusted and the registered token is never sent.
func TestKubectlCLI_MintTrustsOnlyTheRowCA(t *testing.T) {
	api := newKubeAPIServer(t)
	k := kubectlRowDef(api.srv.URL, "", "team-a").CLI.Kubectl
	if _, _, err := mintKubectlToken(context.Background(), *k, kubeRegistered, time.Now()); err == nil {
		t.Fatal("minted against a server the row's trust does not cover")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.seen) != 0 {
		t.Errorf("the untrusted server saw %q", api.seen)
	}
}

// /setup/status tells the console whether the kubectl kind is on, to an admin
// and to a member alike.
func TestKubectlCLI_SetupStatusFeature(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(types.EnvKubectlCLI, map[bool]string{true: "on", false: ""}[on])
		code, st := decodeSetup(t, New(Config{AdminToken: adminToken}), adminToken)
		if code != http.StatusOK || st.Features.KubectlCLI != on {
			t.Errorf("switch %v: status %d features %+v", on, code, st.Features)
		}
		if got := redactSetupStatusForUser(st).Features.KubectlCLI; got != on {
			t.Errorf("switch %v: the member's redaction says %v", on, got)
		}
	}
}
