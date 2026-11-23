// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	kubeHost     = "example.com" // a name the httptest certificate carries
	kubePort     = 6443
	kubeToken    = "Bearer minted-cluster-token"
	kubeSentinel = "Bearer wardyn-proxy-injects-kubectl-token"
)

// kubeHarness is a proxy whose run attaches a kubectl row pinned to the team-a
// and team-b namespaces, a fake API server behind it that records what
// arrives, and a fake control plane that counts every token mint. The
// injector's entry is EXPIRED, so a request that reaches the injector mints.
type kubeHarness struct {
	p      *Proxy
	mu     sync.Mutex
	seen   []string // "METHOD raw-target authorization"
	bodies []string // the body of each request in seen
	minted atomic.Int32
	log    func() string
}

type kubeOpts struct {
	write   bool
	restart bool // the row says restart
	// approveEach: the row says approve_each, set through the wire form as restart is.
	approveEach bool
	caPEM       bool // the grant carries the API server's own certificate as its CA bundle
	trustCA     bool // the egress transport skips verification (the other harnesses' default)
	allowed     []string
}

func newKubeHarness(t *testing.T, o kubeOpts) *kubeHarness {
	t.Helper()
	h := &kubeHarness{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.RequestURI+" "+r.Header.Get("Authorization"))
		b, _ := io.ReadAll(r.Body)
		h.bodies = append(h.bodies, string(b))
		h.mu.Unlock()
		_, _ = io.WriteString(w, `{"kind":"PodList","items":[]}`)
	}))
	t.Cleanup(upstream.Close)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.minted.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: kubeToken, JTI: "kube-jti", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	inj := &injector{
		byHost: map[string]*injEntry{kubeHost: {
			grantID:    uuid.New(),
			header:     injectedHeader{name: "Authorization", value: "Bearer stale"},
			expiresAt:  time.Now().Add(-time.Hour).UnixMilli(),
			requireTLS: true,
			rule:       egress.InjectionRule{RequireTLS: true},
		}},
		base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	if o.allowed == nil {
		o.allowed = []string{kubeHost + ":6443"}
	}
	grant := &KubeAPIGrant{Host: kubeHost, Port: kubePort, Namespaces: []string{"team-a", "team-b"}, Write: o.write}
	if o.restart {
		// Set through the wire form, as the control plane's config reaches the proxy.
		if err := json.Unmarshal([]byte(`{"restart":true}`), grant); err != nil {
			t.Fatal(err)
		}
	}
	if o.approveEach {
		if err := json.Unmarshal([]byte(`{"approve_each":true}`), grant); err != nil {
			t.Fatal(err)
		}
	}
	if o.caPEM {
		grant.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
	}
	var tlsCfg *tls.Config
	if o.trustCA {
		tlsCfg = testInsecureTLSConfig
	}
	buf := &bytes.Buffer{}
	h.p = newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: o.allowed}),
		Injector:        inj,
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: tlsCfg,
		KubeAPI:         grant,
	})
	h.log = func() string {
		_ = h.p.sink.close(context.Background())
		return buf.String()
	}
	return h
}

func (h *kubeHarness) upstreamSaw() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *kubeHarness) tunnel(t *testing.T, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + kubeHost + ":6443\r\nAuthorization: " + kubeSentinel + "\r\n"
	for _, l := range hdr {
		raw += l + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n")))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, kubeHost, kubePort)
	return rec
}

// mustRefuse asserts a 403 Kubernetes Status carrying code, that nothing
// reached the API server and that no token was minted.
func (h *kubeHarness) mustRefuse(t *testing.T, label string, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden || !strings.Contains(body, `"wardyn":"`+code+`"`) || !strings.Contains(body, `"kind":"Status"`) {
		t.Errorf("%s: status %d body %s, want 403 Status %s", label, rec.Code, body, code)
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("%s: the API server saw %q", label, seen)
	}
	if n := h.minted.Load(); n != 0 {
		t.Errorf("%s: %d token(s) minted for a refused request", label, n)
	}
}

// An admitted request reaches the API server with the minted token in place
// of the sandbox's sentinel, on the path the gate compared.
func TestKubeAPI_AdmitsInsideTheRow(t *testing.T) {
	for _, target := range []string{
		"/api/v1/namespaces/team-a/pods?limit=500",
		"/apis/apps/v1/namespaces/team-b/deployments/web",
		"/api/v1/namespaces/team-a/pods/web-1/log?container=app",
		"/api/v1/namespaces/team-a",
		"/api", "/apis", "/api/v1", "/apis/apps", "/apis/apps/v1", "/version", "/openapi/v2", "/openapi/v3/apis/apps/v1",
	} {
		t.Run(target, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{trustCA: true})
			rec := h.tunnel(t, http.MethodGet, target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
			if seen := h.upstreamSaw(); len(seen) != 1 || seen[0] != "GET "+target+" "+kubeToken {
				t.Fatalf("the API server saw %q", seen)
			}
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceKubeAPI+`"`) {
				t.Errorf("decision log lacks %s: %s", ruleSourceKubeAPI, l)
			}
		})
	}
}

// Everything outside the row, and every path that would put a credential or a
// shell in the sandbox, is refused before a token is minted.
func TestKubeAPI_RefusesOutsideTheRow(t *testing.T) {
	type tc struct {
		name, method, target string
		write                bool
		hdr                  []string
	}
	for _, c := range []tc{
		{"a change under read", http.MethodPost, "/api/v1/namespaces/team-a/configmaps", false, nil},
		{"a delete under read", http.MethodDelete, "/apis/apps/v1/namespaces/team-a/deployments/web", false, nil},
		{"another namespace", http.MethodGet, "/api/v1/namespaces/kube-system/pods", true, nil},
		{"a group path in another namespace", http.MethodGet, "/apis/apps/v1/namespaces/prod/deployments", true, nil},
		{"every namespace", http.MethodGet, "/api/v1/pods", true, nil},
		{"the namespace list", http.MethodGet, "/api/v1/namespaces", true, nil},
		{"nodes", http.MethodGet, "/api/v1/nodes", true, nil},
		{"cluster roles", http.MethodGet, "/apis/rbac.authorization.k8s.io/v1/clusterroles", true, nil},
		{"deleting the namespace", http.MethodDelete, "/api/v1/namespaces/team-a", true, nil},
		{"finalizing the namespace", http.MethodPut, "/api/v1/namespaces/team-a/finalize", true, nil},
		{"the namespace's status", http.MethodPut, "/api/v1/namespaces/team-a/status", true, nil},
		{"patching the namespace's status", http.MethodPatch, "/api/v1/namespaces/team-a/status", true, nil},
		{"reading the namespace's status", http.MethodGet, "/api/v1/namespaces/team-a/status", true, nil},
		{"secrets", http.MethodGet, "/api/v1/namespaces/team-a/secrets", true, nil},
		{"one secret", http.MethodGet, "/api/v1/namespaces/team-a/secrets/db", true, nil},
		{"a token request", http.MethodPost, "/api/v1/namespaces/team-a/serviceaccounts/default/token", true, nil},
		{"exec", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/exec?command=sh", true, nil},
		{"exec over GET", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/exec?command=sh", true, nil},
		{"attach", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/attach", true, nil},
		{"port-forward", http.MethodPost, "/api/v1/namespaces/team-a/pods/web-1/portforward", true, nil},
		{"the pod proxy", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/proxy", true, nil},
		{"the pod proxy with a path", http.MethodGet, "/api/v1/namespaces/team-a/services/web/proxy/admin", true, nil},
		{"ephemeral containers", http.MethodPatch, "/api/v1/namespaces/team-a/pods/web-1/ephemeralcontainers", true, nil},
		{"legacy watch", http.MethodGet, "/api/v1/watch/namespaces/team-a/pods", true, nil},
		{"an upgrade", http.MethodGet, "/api/v1/namespaces/team-a/pods/web-1/log", true, []string{"Connection: Upgrade", "Upgrade: websocket"}},
		{"impersonation", http.MethodGet, "/api/v1/namespaces/team-a/pods", true, []string{"Impersonate-User: system:admin"}},
		{"impersonated groups", http.MethodGet, "/api/v1/namespaces/team-a/pods", true, []string{"Impersonate-Group: system:masters"}},
		{"an encoded slash", http.MethodGet, "/api/v1/namespaces/team-a%2f..%2fkube-system/pods", true, nil},
		{"a dot-dot segment", http.MethodGet, "/api/v1/namespaces/team-a/../kube-system/pods", true, nil},
		{"an empty segment", http.MethodGet, "/api/v1/namespaces//pods", true, nil},
		{"a discovery write", http.MethodPost, "/apis", true, nil},
		{"another method", "OPTIONS", "/api/v1/namespaces/team-a/pods", true, nil},
		{"health", http.MethodGet, "/healthz", true, nil},
		{"the root", http.MethodGet, "/", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{write: c.write, trustCA: true})
			h.mustRefuse(t, c.name, h.tunnel(t, c.method, c.target, c.hdr...), kubeAPIRefused)
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceKubeAPIRefused+`"`) {
				t.Errorf("decision log lacks %s: %s", ruleSourceKubeAPIRefused, l)
			}
		})
	}
}

// A row that says write may change its own namespaces. A workload's body is
// judged by TestKubeAPI_PodSpecWrites.
func TestKubeAPI_WriteInsideTheRow(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
		if rec := h.tunnel(t, m, "/api/v1/namespaces/team-b/configmaps/web"); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d body %s", m, rec.Code, rec.Body)
		}
	}
}

// A write of a pod, or of a workload's pod template, is forwarded only when it
// sets automountServiceAccountToken: false, mounts only plain volumes, reads no
// Secret and uses no host access: what the pod prints is readable through its
// log. A patch that touches the pod spec is refused, since the door cannot see
// what the object already mounts.
func TestKubeAPI_PodSpecWrites(t *testing.T) {
	const (
		ct      = "Content-Type: application/json"
		mergeCT = "Content-Type: application/merge-patch+json"
		pods    = "/api/v1/namespaces/team-a/pods"
		ok      = `"automountServiceAccountToken":false,"containers":[{"name":"app","image":"busybox"}]`
	)
	pod := func(spec string) string {
		return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{` + spec + `}}`
	}
	type tc struct {
		name, method, target, body string
		write, admitted            bool
		hdr                        []string
	}
	for _, c := range []tc{
		{"no automount field", http.MethodPost, pods, pod(`"containers":[{"name":"app","image":"busybox"}]`), true, false, []string{ct}},
		{"automount true", http.MethodPost, pods, pod(`"automountServiceAccountToken":true,"containers":[]`), true, false, []string{ct}},
		{"automount false, no secrets", http.MethodPost, pods, pod(ok), true, true, []string{ct}},
		{"a replace with automount false", http.MethodPut, pods + "/p", pod(ok), true, true, []string{ct}},
		{"a secret volume", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"s","secret":{"secretName":"db"}}]`), true, false, []string{ct}},
		{"a projected secret", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"s","projected":{"sources":[{"secret":{"name":"db"}}]}}]`), true, false, []string{ct}},
		{"a projected serviceAccountToken", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"t","projected":{"sources":[{"serviceAccountToken":{"path":"token"}}]}}]`), true, false, []string{ct}},
		{"secretKeyRef env", http.MethodPost, pods, pod(`"automountServiceAccountToken":false,"containers":[{"name":"app","env":[{"name":"P","valueFrom":{"secretKeyRef":{"name":"db","key":"p"}}}]}]`), true, false, []string{ct}},
		{"envFrom secretRef", http.MethodPost, pods, pod(`"automountServiceAccountToken":false,"initContainers":[{"name":"i","envFrom":[{"secretRef":{"name":"db"}}]}]`), true, false, []string{ct}},
		{"a key in another case", http.MethodPost, pods, pod(ok + `,"Volumes":[{"name":"s","Secret":{"secretName":"db"}}]`), true, false, []string{ct}},
		{"a repeated automount key", http.MethodPost, pods, pod(ok + `,"AutomountServiceAccountToken":true`), true, false, []string{ct}},
		{"a Deployment template with a secret volume", http.MethodPost, "/apis/apps/v1/namespaces/team-b/deployments",
			`{"kind":"Deployment","spec":{"template":{"spec":{` + ok + `,"volumes":[{"name":"s","secret":{"secretName":"db"}}]}}}}`, true, false, []string{ct}},
		{"a Deployment template that is clean", http.MethodPost, "/apis/apps/v1/namespaces/team-b/deployments",
			`{"kind":"Deployment","spec":{"template":{"spec":{` + ok + `}}}}`, true, true, []string{ct}},
		{"a CronJob nested template", http.MethodPost, "/apis/batch/v1/namespaces/team-a/cronjobs",
			`{"kind":"CronJob","spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"app"}]}}}}}}`, true, false, []string{ct}},
		{"a pod template", http.MethodPost, "/api/v1/namespaces/team-a/podtemplates", `{"kind":"PodTemplate","template":{"spec":{}}}`, true, false, []string{ct}},
		// Volumes are an allow-list: a kind outside it, a future one among them, is refused.
		{"a hostPath volume", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"h","hostPath":{"path":"/var/lib/kubelet/pods"}}]`), true, false, []string{ct}},
		{"a csi volume with nodePublishSecretRef", http.MethodPost, pods,
			pod(ok + `,"volumes":[{"name":"c","csi":{"driver":"secrets-store.csi.k8s.io","nodePublishSecretRef":{"name":"db"}}}]`), true, false, []string{ct}},
		{"a cephfs volume with secretRef", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"c","cephfs":{"monitors":["x"],"secretRef":{"name":"db"}}}]`), true, false, []string{ct}},
		{"a projected podCertificate", http.MethodPost, pods,
			pod(ok + `,"volumes":[{"name":"c","projected":{"sources":[{"podCertificate":{"signerName":"kubernetes.io/kube-apiserver-client-pod"}}]}}]`), true, false, []string{ct}},
		{"an unknown volume kind", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"u","futureVolume":{}}]`), true, false, []string{ct}},
		{"an emptyDir volume", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"e","emptyDir":{}}]`), true, true, []string{ct}},
		{"a configMap volume", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"c","configMap":{"name":"cfg"}}]`), true, true, []string{ct}},
		{"a persistentVolumeClaim volume", http.MethodPost, pods, pod(ok + `,"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"data"}}]`), true, true, []string{ct}},
		{"a downwardAPI volume", http.MethodPost, pods,
			pod(ok + `,"volumes":[{"name":"d","downwardAPI":{"items":[{"path":"n","fieldRef":{"fieldPath":"metadata.name"}}]}}]`), true, true, []string{ct}},
		{"a projected configMap and clusterTrustBundle", http.MethodPost, pods,
			pod(ok + `,"volumes":[{"name":"p","projected":{"sources":[{"configMap":{"name":"cfg"}},{"clusterTrustBundle":{"name":"x","path":"ca"}}]}}]`), true, true, []string{ct}},
		{"an ephemeral and an image volume", http.MethodPost, pods,
			pod(ok + `,"volumes":[{"name":"s","ephemeral":{"volumeClaimTemplate":{"spec":{}}}},{"name":"i","image":{"reference":"busybox"}}]`), true, true, []string{ct}},
		// Host access: each of these can read other pods' token files on the node.
		{"hostPID", http.MethodPost, pods, pod(ok + `,"hostPID":true`), true, false, []string{ct}},
		{"hostIPC", http.MethodPost, pods, pod(ok + `,"hostIPC":true`), true, false, []string{ct}},
		{"hostNetwork", http.MethodPost, pods, pod(ok + `,"hostNetwork":true`), true, false, []string{ct}},
		{"a privileged container", http.MethodPost, pods,
			pod(`"automountServiceAccountToken":false,"containers":[{"name":"a","securityContext":{"privileged":true}}]`), true, false, []string{ct}},
		{"a privileged init container", http.MethodPost, pods, pod(ok + `,"initContainers":[{"name":"i","securityContext":{"privileged":true}}]`), true, false, []string{ct}},
		{"a privileged ephemeral container", http.MethodPost, pods, pod(ok + `,"ephemeralContainers":[{"name":"e","securityContext":{"privileged":true}}]`), true, false, []string{ct}},
		{"host access set false", http.MethodPost, pods,
			pod(`"automountServiceAccountToken":false,"hostPID":false,"hostIPC":false,"hostNetwork":false,"containers":[{"name":"a","securityContext":{"privileged":false}}]`), true, true, []string{ct}},
		// A Windows host-process container runs on the node itself, set for the pod or for one container.
		{"a pod-level Windows hostProcess", http.MethodPost, pods, pod(ok + `,"securityContext":{"windowsOptions":{"hostProcess":true}}`), true, false, []string{ct}},
		{"a container-level Windows hostProcess", http.MethodPost, pods,
			pod(`"automountServiceAccountToken":false,"containers":[{"name":"a","securityContext":{"windowsOptions":{"hostProcess":true}}}]`), true, false, []string{ct}},
		{"a host-process init container", http.MethodPost, pods,
			pod(ok + `,"initContainers":[{"name":"i","securityContext":{"windowsOptions":{"hostProcess":true}}}]`), true, false, []string{ct}},
		{"a host-process ephemeral container", http.MethodPost, pods,
			pod(ok + `,"ephemeralContainers":[{"name":"e","securityContext":{"windowsOptions":{"hostProcess":true}}}]`), true, false, []string{ct}},
		{"hostProcess as a string", http.MethodPost, pods, pod(ok + `,"securityContext":{"windowsOptions":{"hostProcess":"true"}}`), true, false, []string{ct}},
		{"windowsOptions as a list", http.MethodPost, pods,
			pod(`"automountServiceAccountToken":false,"containers":[{"name":"a","securityContext":{"windowsOptions":[]}}]`), true, false, []string{ct}},
		{"hostProcess set false", http.MethodPost, pods,
			pod(`"automountServiceAccountToken":false,"securityContext":{"windowsOptions":{"hostProcess":false}},"containers":[{"name":"a","securityContext":{"windowsOptions":{"hostProcess":false}}}]`), true, true, []string{ct}},
		{"a YAML body", http.MethodPost, pods, pod(ok), true, false, []string{"Content-Type: application/yaml"}},
		{"an encoded body", http.MethodPost, pods, pod(ok), true, false, []string{ct, "Content-Encoding: gzip"}},
		{"a JSON Patch to pods", http.MethodPatch, pods + "/p", `[{"op":"add","path":"/metadata/labels/a","value":"b"}]`, true, false,
			[]string{"Content-Type: application/json-patch+json"}},
		{"a merge patch adding a secret volume", http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments/web",
			`{"spec":{"template":{"spec":{"automountServiceAccountToken":false,"volumes":[{"name":"s","secret":{"secretName":"db"}}]}}}}`, true, false, []string{mergeCT}},
		{"a merge patch to the pod spec", http.MethodPatch, pods + "/p", `{"spec":{"containers":[{"name":"app","image":"evil"}]}}`, true, false, []string{mergeCT}},
		{"a merge patch that nulls the template", http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments/web", `{"spec":{"template":null}}`, true, false, []string{mergeCT}},
		{"a strategic merge directive", http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments/web", `{"spec":{"$patch":"replace","replicas":1}}`, true, false,
			[]string{"Content-Type: application/strategic-merge-patch+json"}},
		{"a merge patch to labels", http.MethodPatch, pods + "/p", `{"metadata":{"labels":{"a":"b"}}}`, true, true, []string{mergeCT}},
		{"a merge patch to replicas", http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments/web", `{"spec":{"replicas":2}}`, true, true, []string{mergeCT}},
		{"an over-cap body", http.MethodPost, pods, pod(ok + `,"hostname":"` + strings.Repeat("a", kubePodSpecMaxBody) + `"`), true, false, []string{ct}},
		{"a read-only grant", http.MethodPost, pods, pod(ok), false, false, []string{ct}},
		{"a configmap is not judged", http.MethodPost, "/api/v1/namespaces/team-a/configmaps", `{"data":{"a":"b"}}`, true, true, []string{ct}},
		{"GET pods log", http.MethodGet, pods + "/p/log", "", true, true, nil},
		{"DELETE a deployment", http.MethodDelete, "/apis/apps/v1/namespaces/team-b/deployments/web", "", true, true, nil},
		// A StatefulSet builds its pods from a ControllerRevision's data, which the door does not read.
		{"POST a ControllerRevision", http.MethodPost, "/apis/apps/v1/namespaces/team-a/controllerrevisions",
			`{"kind":"ControllerRevision","revision":1,"data":{"spec":{"template":{"$patch":"replace","spec":{"volumes":[{"name":"s","secret":{"secretName":"db"}}]}}}}}`, true, false, []string{ct}},
		{"PATCH a ControllerRevision", http.MethodPatch, "/apis/apps/v1/namespaces/team-a/controllerrevisions/web-1", `{"data":{"spec":{}}}`, true, false, []string{mergeCT}},
		{"PUT a ControllerRevision", http.MethodPut, "/apis/apps/v1/namespaces/team-a/controllerrevisions/web-1", `{"kind":"ControllerRevision","revision":2}`, true, false, []string{ct}},
		{"GET a ControllerRevision", http.MethodGet, "/apis/apps/v1/namespaces/team-a/controllerrevisions/web-1", "", true, true, nil},
		{"DELETE a ControllerRevision", http.MethodDelete, "/apis/apps/v1/namespaces/team-a/controllerrevisions/web-1", "", true, true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{write: c.write, trustCA: true})
			rec := h.tunnelBody(t, c.method, c.target, c.body, c.hdr...)
			if !c.admitted {
				h.mustRefuse(t, c.name, rec, kubeAPIRefused)
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.bodies) != 1 || h.bodies[0] != c.body {
				t.Errorf("the API server got %q, want the judged body", h.bodies)
			}
		})
	}
}

// The API server reads keys case-sensitively and ignores one spelled
// otherwise, so a key the door reads that is spelled in another case is
// refused as such, never read as the key: a mis-cased automount key alone
// leaves the token mounted.
func TestKubeAPI_PodSpecMisCasedKeys(t *testing.T) {
	const (
		pods = "/api/v1/namespaces/team-a/pods"
		cs   = `"containers":[{"name":"app","image":"busybox"}]`
		ok   = `"automountServiceAccountToken":false,` + cs
	)
	pod := func(spec string) string { return `{"kind":"Pod","spec":{` + spec + `}}` }
	for name, c := range map[string]struct{ method, target, body, ct string }{
		"AutomountServiceAccountToken alone": {http.MethodPost, pods, pod(`"AutomountServiceAccountToken":false,` + cs), "application/json"},
		"automountserviceaccounttoken alone": {http.MethodPost, pods, pod(`"automountserviceaccounttoken":false,` + cs), "application/json"},
		"Spec on a pod":                      {http.MethodPost, pods, `{"kind":"Pod","Spec":{` + ok + `}}`, "application/json"},
		"Template on a deployment": {http.MethodPost, "/apis/apps/v1/namespaces/team-a/deployments",
			`{"kind":"Deployment","spec":{"Template":{"spec":{` + ok + `}}}}`, "application/json"},
		"jobtemplate on a cronjob": {http.MethodPost, "/apis/batch/v1/namespaces/team-a/cronjobs",
			`{"kind":"CronJob","spec":{"jobtemplate":{"spec":{"template":{"spec":{` + ok + `}}}}}}`, "application/json"},
		"HostPID":               {http.MethodPost, pods, pod(ok + `,"HostPID":true`), "application/json"},
		"Privileged":            {http.MethodPost, pods, pod(`"automountServiceAccountToken":false,"containers":[{"name":"a","securityContext":{"Privileged":true}}]`), "application/json"},
		"HostProcess":           {http.MethodPost, pods, pod(ok + `,"securityContext":{"windowsOptions":{"HostProcess":true}}`), "application/json"},
		"WindowsOptions":        {http.MethodPost, pods, pod(`"automountServiceAccountToken":false,"containers":[{"name":"a","securityContext":{"WindowsOptions":{"hostProcess":true}}}]`), "application/json"},
		"Spec in a merge patch": {http.MethodPatch, pods + "/p", `{"Spec":{"containers":[]}}`, "application/merge-patch+json"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
			rec := h.tunnelBody(t, c.method, c.target, c.body, "Content-Type: "+c.ct)
			h.mustRefuse(t, name, rec, kubeAPIRefused)
			if !strings.Contains(rec.Body.String(), errKubePodCase.Error()) {
				t.Errorf("refused for another reason: %s", rec.Body)
			}
		})
	}
}

// Every resource the door reads a pod spec from: a clean body at the
// resource's own path is admitted, and the same body with a Secret volume is
// refused. The bodies are written out here, not built from kubePodSpecPaths,
// so a wrong path in that table refuses the clean body.
func TestKubeAPI_PodSpecEveryResource(t *testing.T) {
	const (
		clean  = `"automountServiceAccountToken":false,"containers":[{"name":"app","image":"busybox"}]`
		secret = clean + `,"volumes":[{"name":"s","secret":{"secretName":"db"}}]`
	)
	tmpl := `{"spec":{"template":{"spec":{SPEC}}}}`
	rows := map[string]struct{ target, body string }{
		"pods":                   {"/api/v1/namespaces/team-a/pods", `{"kind":"Pod","spec":{SPEC}}`},
		"deployments":            {"/apis/apps/v1/namespaces/team-a/deployments", tmpl},
		"replicasets":            {"/apis/apps/v1/namespaces/team-a/replicasets", tmpl},
		"statefulsets":           {"/apis/apps/v1/namespaces/team-a/statefulsets", tmpl},
		"daemonsets":             {"/apis/apps/v1/namespaces/team-a/daemonsets", tmpl},
		"jobs":                   {"/apis/batch/v1/namespaces/team-a/jobs", tmpl},
		"replicationcontrollers": {"/api/v1/namespaces/team-a/replicationcontrollers", tmpl},
		"cronjobs":               {"/apis/batch/v1/namespaces/team-a/cronjobs", `{"kind":"CronJob","spec":{"jobTemplate":{"spec":{"template":{"spec":{SPEC}}}}}}`},
		"podtemplates":           {"/api/v1/namespaces/team-a/podtemplates", `{"kind":"PodTemplate","template":{"spec":{SPEC}}}`},
	}
	if len(rows) != len(kubePodSpecPaths) {
		t.Errorf("%d bodies for %d resources in kubePodSpecPaths", len(rows), len(kubePodSpecPaths))
	}
	for res := range kubePodSpecPaths {
		t.Run(res, func(t *testing.T) {
			row, ok := rows[res]
			if !ok {
				t.Fatalf("no body for %s", res)
			}
			h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
			if rec := h.tunnelBody(t, http.MethodPost, row.target, strings.Replace(row.body, "SPEC", clean, 1), "Content-Type: application/json"); rec.Code != http.StatusOK {
				t.Errorf("clean: status %d body %s", rec.Code, rec.Body)
			}
			h = newKubeHarness(t, kubeOpts{write: true, trustCA: true})
			rec := h.tunnelBody(t, http.MethodPost, row.target, strings.Replace(row.body, "SPEC", secret, 1), "Content-Type: application/json")
			h.mustRefuse(t, res+" with a Secret volume", rec, kubeAPIRefused)
		})
	}
}

// A restart-only row keeps its own judge and its 4 KiB cap: the pod-spec check
// runs for write alone, so a refused patch names the restart rule.
func TestKubeAPI_RestartRowSkipsPodSpec(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{restart: true, trustCA: true})
	rec := h.tunnelBody(t, http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments/web",
		`[{"op":"add","path":"/spec/replicas","value":2}]`, "Content-Type: application/json-patch+json")
	h.mustRefuse(t, "a JSON Patch on a restart row", rec, kubeAPIRefused)
	if !strings.Contains(rec.Body.String(), kubeRestartRefusalLead) {
		t.Errorf("refused by another rule: %s", rec.Body)
	}
}

// The plain lane and a tunnel on another port never reach the door.
func TestKubeAPI_PlainLaneAndOtherPortRefused(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://"+kubeHost+":6443/api/v1/namespaces/team-a/pods")
	req.Header.Set("Authorization", kubeSentinel)
	h.p.servePlain(rec, req)
	h.mustRefuse(t, "plain", rec, kubeAPIRefused)

	h = newKubeHarness(t, kubeOpts{write: true, trustCA: true, allowed: []string{kubeHost + ":443"}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodConnect, "http://"+kubeHost+":443", nil)
	req.Host = kubeHost + ":443"
	h.p.handleConnect(rec, req)
	h.mustRefuse(t, "connect :443", rec, kubeAPIRefused)
}

// The door trusts the row's CA bundle: with it, a server the egress trust set
// does not know is reached; without it, the same server is not.
func TestKubeAPI_TrustsTheRowCA(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{caPEM: true})
	if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code != http.StatusOK {
		t.Fatalf("with the row's CA: status %d body %s", rec.Code, rec.Body)
	}
	h = newKubeHarness(t, kubeOpts{})
	if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code == http.StatusOK {
		t.Fatalf("without the row's CA the unknown server was trusted")
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("the untrusted server saw %q", seen)
	}
}

func TestKubeAPI_ConfigValidation(t *testing.T) {
	const tokenName = "wardyn-cli-1b4e28ba-2fa1-11d2-883f-0016d3cca427-kubectl"
	rule := func(host string) InjectionConfig {
		return InjectionConfig{InjectionRule: egress.InjectionRule{Host: host, Header: "Authorization", SecretName: tokenName, RequireTLS: true}, GrantID: uuid.New()}
	}
	good := func() *Config {
		return &Config{MITMCACertPEM: "cert", MITMCAKeyPEM: "key",
			KubeAPI:   &KubeAPIGrant{Host: "k8s.example.com", Port: 6443, Namespaces: []string{"team-a"}},
			Injection: []InjectionConfig{rule("k8s.example.com")}}
	}
	// validateTerminatedGates runs every door's check, the Azure CLI's among them,
	// which must leave a kubectl token rule to this door.
	if err := good().validateTerminatedGates(); err != nil {
		t.Fatalf("a well-formed grant = %v", err)
	}
	if err := (&Config{Injection: []InjectionConfig{rule("k8s.example.com")}}).validateKubeAPI(); err == nil {
		t.Error("a cluster token rule with no kube_api was accepted")
	}
	for name, mutate := range map[string]func(*Config){
		"no CA":               func(c *Config) { c.MITMCAKeyPEM = "" },
		"no host":             func(c *Config) { c.KubeAPI.Host = "" },
		"a host with a path":  func(c *Config) { c.KubeAPI.Host = "k8s.example.com/x" },
		"no port":             func(c *Config) { c.KubeAPI.Port = 0 },
		"no namespace":        func(c *Config) { c.KubeAPI.Namespaces = nil },
		"a bad namespace":     func(c *Config) { c.KubeAPI.Namespaces = []string{"A"} },
		"a namespace twice":   func(c *Config) { c.KubeAPI.Namespaces = []string{"a", "a"} },
		"a CA that is not":    func(c *Config) { c.KubeAPI.CAPEM = "nope" },
		"no rule":             func(c *Config) { c.Injection = nil },
		"two rules":           func(c *Config) { c.Injection = append(c.Injection, rule("K8S.example.com.")) },
		"a rule without TLS":  func(c *Config) { c.Injection[0].RequireTLS = false },
		"a pinned rule":       func(c *Config) { c.Injection[0].PinPath = "/api" },
		"token on other host": func(c *Config) { c.Injection = append(c.Injection, rule("evil.example")) },
	} {
		c := good()
		mutate(c)
		if err := c.validateKubeAPI(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
