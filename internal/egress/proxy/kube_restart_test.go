// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The request `kubectl rollout restart deployment/web -n team-a` sends,
// verified against kubectl v0.32.2: pkg/cmd/rollout/rollout_restart.go builds
// the patch (a two-way strategic merge of the object with the annotation
// added) and sends it through resource.Helper.Patch with the field manager
// "kubectl-rollout" and types.StrategicMergePatchType; the annotation is set by
// pkg/polymorphichelpers/objectrestarter.go, value time.Now().Format(RFC3339).
const (
	restartTarget   = "/apis/apps/v1/namespaces/team-a/deployments/web?fieldManager=kubectl-rollout"
	restartBody     = `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2026-11-07T18:30:00Z"}}}}}`
	restartPatchCT  = "application/strategic-merge-patch+json"
	restartCTHeader = "Content-Type: " + restartPatchCT
)

// tunnelBody is tunnel with a request body. Content-Length is added unless hdr
// carries a Transfer-Encoding line, which sends the body chunked.
func (h *kubeHarness) tunnelBody(t *testing.T, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + kubeHost + ":6443\r\nAuthorization: " + kubeSentinel + "\r\n"
	chunked := false
	for _, l := range hdr {
		raw += l + "\r\n"
		chunked = chunked || strings.HasPrefix(l, "Transfer-Encoding")
	}
	if chunked {
		raw += "\r\n" + fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	} else {
		raw += fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, kubeHost, kubePort)
	return rec
}

func restartHarness(t *testing.T) *kubeHarness {
	t.Helper()
	return newKubeHarness(t, kubeOpts{restart: true, trustCA: true})
}

// The exact request kubectl sends is the one write a restart row admits, and
// the API server receives the body byte for byte.
func TestKubeDoorRestartAdmitsExactPatch(t *testing.T) {
	for name, c := range map[string]struct {
		target string
		hdr    []string
	}{
		"as kubectl sends it":        {restartTarget, []string{restartCTHeader}},
		"no query":                   {"/apis/apps/v1/namespaces/team-a/deployments/web", []string{restartCTHeader}},
		"the other listed namespace": {"/apis/apps/v1/namespaces/team-b/deployments/web?fieldManager=kubectl-rollout", []string{restartCTHeader}},
		"a media type parameter":     {restartTarget, []string{restartCTHeader + "; charset=utf-8"}},
		"the media type in capitals": {restartTarget, []string{"Content-Type: Application/Strategic-Merge-Patch+JSON"}},
		"identity encoding":          {restartTarget, []string{restartCTHeader, "Content-Encoding: identity"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			rec := h.tunnelBody(t, http.MethodPatch, c.target, restartBody, c.hdr...)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body)
			}
			if seen := h.upstreamSaw(); len(seen) != 1 || seen[0] != "PATCH "+c.target+" "+kubeToken {
				t.Fatalf("the API server saw %q", seen)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.bodies) != 1 || h.bodies[0] != restartBody {
				t.Fatalf("the API server received the body %q, want %q", h.bodies, restartBody)
			}
		})
	}
}

// A restart row reads like a read row, and its restart is not a write: every
// other change is still refused, and the refusal says what the row may do.
func TestKubeDoorRestartReadsLikeRead(t *testing.T) {
	h := restartHarness(t)
	for _, target := range []string{
		"/api/v1/namespaces/team-a/pods?limit=500", "/apis/apps/v1/namespaces/team-b/deployments/web", "/apis", "/version",
	} {
		if rec := h.tunnel(t, http.MethodGet, target); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d body %s", target, rec.Code, rec.Body)
		}
	}
	h = restartHarness(t)
	rec := h.tunnelBody(t, http.MethodPost, "/api/v1/namespaces/team-a/configmaps", `{}`, "Content-Type: application/json")
	h.mustRefuse(t, "a create", rec, kubeAPIRefused)
	if !strings.Contains(rec.Body.String(), "may only restart") {
		t.Errorf("the refusal does not say what the row allows: %s", rec.Body)
	}
}

// The control: a read row still refuses the same request.
func TestKubeDoorRestartReadRowStillRefuses(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{trustCA: true})
	rec := h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, restartCTHeader)
	h.mustRefuse(t, "a read row", rec, kubeAPIRefused)
}

func restartWith(annotations string) string {
	return `{"spec":{"template":{"metadata":{"annotations":{` + annotations + `}}}}}`
}

const restartKey = `"kubectl.kubernetes.io/restartedAt":"2026-11-07T18:30:00Z"`

// Anything in the patch beyond the one annotation is refused.
func TestKubeDoorRestartRefusesOtherPatchContent(t *testing.T) {
	for name, body := range map[string]string{
		"an extra annotation":         restartWith(restartKey + `,"example.com/x":"y"`),
		"another annotation instead":  restartWith(`"example.com/x":"y"`),
		"a repeated key":              restartWith(restartKey + `,` + restartKey),
		"a repeated key, other value": restartWith(restartKey + `,"kubectl.kubernetes.io/restartedAt":"2026-11-07T18:31:00Z"`),
		"an image change":             `{"spec":{"template":{"spec":{"containers":[{"name":"web","image":"evil/x@sha256:00"}]}}}}`,
		"the image beside it":         `{"spec":{"template":{"metadata":{"annotations":{` + restartKey + `}},"spec":{"containers":[{"name":"web","image":"evil/x"}]}}}}`,
		"replicas beside it":          `{"spec":{"replicas":0,"template":{"metadata":{"annotations":{` + restartKey + `}}}}}`,
		"a label beside it":           `{"spec":{"template":{"metadata":{"labels":{"a":"b"},"annotations":{` + restartKey + `}}}}}`,
		"metadata beside spec":        `{"metadata":{"labels":{"a":"b"}},"spec":{"template":{"metadata":{"annotations":{` + restartKey + `}}}}}`,
		"a finalizer":                 `{"metadata":{"finalizers":["x"]},"spec":{"template":{"metadata":{"annotations":{` + restartKey + `}}}}}`,
		"a null value":                restartWith(`"kubectl.kubernetes.io/restartedAt":null`),
		"a number value":              restartWith(`"kubectl.kubernetes.io/restartedAt":1`),
		"an object value":             restartWith(`"kubectl.kubernetes.io/restartedAt":{}`),
		"a value that is no time":     restartWith(`"kubectl.kubernetes.io/restartedAt":"now; rm -rf /"`),
		"an empty value":              restartWith(`"kubectl.kubernetes.io/restartedAt":""`),
		"no annotations":              `{"spec":{"template":{"metadata":{}}}}`,
		"an empty object":             `{}`,
		"an empty body":               ``,
		"not JSON":                    `spec: template`,
		"trailing data":               restartBody + `{}`,
		"an array":                    `[` + restartBody + `]`,
		"a json-patch list":           `[{"op":"add","path":"/spec/replicas","value":0}]`,
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, restartTarget, body, restartCTHeader), kubeAPIRefused)
		})
	}
}

// A query that changes what the server does with the patch is refused.
func TestKubeDoorRestartRefusesQuery(t *testing.T) {
	base := "/apis/apps/v1/namespaces/team-a/deployments/web"
	for name, q := range map[string]string{
		"dryRun":                    "?dryRun=All",
		"dryRun beside the manager": "?fieldManager=kubectl-rollout&dryRun=All",
		"force":                     "?force=true",
		"force beside the manager":  "?fieldManager=kubectl-rollout&force=true",
		"another manager":           "?fieldManager=someone-else",
		"the manager twice":         "?fieldManager=kubectl-rollout&fieldManager=kubectl-rollout",
		"a field validation":        "?fieldValidation=Ignore",
		"an unknown key":            "?x=1",
		"a bare key":                "?dryRun",
		"a semicolon":               "?fieldManager=kubectl-rollout;dryRun=All",
		"an encoded key":            "?dry%52un=All",
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, base+q, restartBody, restartCTHeader), kubeAPIRefused)
		})
	}
}

// Only the strategic merge patch kubectl sends is admitted, by parsed media type.
func TestKubeDoorRestartRefusesOtherContentType(t *testing.T) {
	for name, hdr := range map[string][]string{
		"a json-patch":          {"Content-Type: application/json-patch+json"},
		"a merge-patch":         {"Content-Type: application/merge-patch+json"},
		"an apply-patch":        {"Content-Type: application/apply-patch+yaml"},
		"plain json":            {"Content-Type: application/json"},
		"no content type":       nil,
		"an empty content type": {"Content-Type: "},
		"a suffixed lookalike":  {"Content-Type: application/strategic-merge-patch+json-x"},
		"a prefixed lookalike":  {"Content-Type: x-application/strategic-merge-patch+json"},
		"an unparseable type":   {"Content-Type: application/strategic-merge-patch+json; charset"},
		"two content types":     {restartCTHeader, "Content-Type: application/json"},
		"two, the bad first":    {"Content-Type: application/json", restartCTHeader},
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, hdr...), kubeAPIRefused)
		})
	}
}

// Only a Deployment in the apps/v1 group, named, is restarted.
func TestKubeDoorRestartRefusesOtherResource(t *testing.T) {
	for name, path := range map[string]string{
		"statefulsets":              "/apis/apps/v1/namespaces/team-a/statefulsets/db",
		"daemonsets":                "/apis/apps/v1/namespaces/team-a/daemonsets/agent",
		"pods":                      "/api/v1/namespaces/team-a/pods/web-1",
		"replicasets":               "/apis/apps/v1/namespaces/team-a/replicasets/web-abc",
		"deployments/scale":         "/apis/apps/v1/namespaces/team-a/deployments/web/scale",
		"deployments/status":        "/apis/apps/v1/namespaces/team-a/deployments/web/status",
		"the collection":            "/apis/apps/v1/namespaces/team-a/deployments",
		"configmaps":                "/api/v1/namespaces/team-a/configmaps/c",
		"cronjobs":                  "/apis/batch/v1/namespaces/team-a/cronjobs/c",
		"services":                  "/api/v1/namespaces/team-a/services/web",
		"apps/v1beta1":              "/apis/apps/v1beta1/namespaces/team-a/deployments/web",
		"extensions/v1beta1":        "/apis/extensions/v1beta1/namespaces/team-a/deployments/web",
		"another group":             "/apis/example.com/v1/namespaces/team-a/deployments/web",
		"the core group":            "/api/v1/namespaces/team-a/deployments/web",
		"an unlisted namespace":     "/apis/apps/v1/namespaces/prod/deployments/web",
		"the kube-system namespace": "/apis/apps/v1/namespaces/kube-system/deployments/coredns",
		"an encoded name":           "/apis/apps/v1/namespaces/team-a/deployments/we%62",
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, path+"?fieldManager=kubectl-rollout", restartBody, restartCTHeader), kubeAPIRefused)
		})
	}
}

// No other verb is admitted, on the restart path or any other.
func TestKubeDoorRestartRefusesOtherVerbs(t *testing.T) {
	named := "/apis/apps/v1/namespaces/team-a/deployments/web"
	for name, c := range map[string]struct{ method, target string }{
		"POST on the collection":   {http.MethodPost, "/apis/apps/v1/namespaces/team-a/deployments"},
		"POST on the object":       {http.MethodPost, named},
		"PUT on the object":        {http.MethodPut, named},
		"DELETE on the object":     {http.MethodDelete, named},
		"DELETE on the collection": {http.MethodDelete, "/apis/apps/v1/namespaces/team-a/deployments"},
		"PATCH on the collection":  {http.MethodPatch, "/apis/apps/v1/namespaces/team-a/deployments"},
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, c.method, c.target+"?fieldManager=kubectl-rollout", restartBody, restartCTHeader), kubeAPIRefused)
		})
	}
	t.Run("DELETE without a body", func(t *testing.T) {
		h := restartHarness(t)
		h.mustRefuse(t, "DELETE", h.tunnel(t, http.MethodDelete, named), kubeAPIRefused)
	})
}

// A body over 4 KiB is refused whole, never truncated, whether its length is
// declared or the request is chunked. The padding is white space, so only the
// cap refuses it.
func TestKubeDoorRestartRefusesOversizeBody(t *testing.T) {
	big := restartBody + strings.Repeat(" ", 4096)
	for name, hdr := range map[string][]string{
		"declared": {restartCTHeader},
		"chunked":  {restartCTHeader, "Transfer-Encoding: chunked"},
	} {
		t.Run(name, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, restartTarget, big, hdr...), kubeAPIRefused)
		})
	}
	// The same body at the cap is admitted: the limit is a limit, not a refusal of all padding.
	h := restartHarness(t)
	ok := restartBody + strings.Repeat(" ", 4096-len(restartBody))
	if rec := h.tunnelBody(t, http.MethodPatch, restartTarget, ok, restartCTHeader); rec.Code != http.StatusOK {
		t.Errorf("a body of exactly 4096 bytes: status %d body %s", rec.Code, rec.Body)
	}
}

// A body the proxy cannot read as sent (compressed) is refused.
func TestKubeDoorRestartRefusesContentEncoding(t *testing.T) {
	for _, enc := range []string{"gzip", "deflate", "br", "zstd", "identity, gzip", "gzip, identity", "x-unknown"} {
		t.Run(enc, func(t *testing.T) {
			h := restartHarness(t)
			h.mustRefuse(t, enc, h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, restartCTHeader, "Content-Encoding: "+enc), kubeAPIRefused)
		})
	}
}

// What the proxy checked is what it sends: the refusal comes before a token is
// minted, and a trailing header such as an impersonation header still refuses.
func TestKubeDoorRestartKeepsTheOtherGates(t *testing.T) {
	h := restartHarness(t)
	h.mustRefuse(t, "impersonation", h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, restartCTHeader, "Impersonate-User: admin"), kubeAPIRefused)
	h = restartHarness(t)
	h.mustRefuse(t, "upgrade", h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, restartCTHeader, "Upgrade: SPDY/3.1", "Connection: Upgrade"), kubeAPIRefused)
}
