// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	kcCluster = "k8s.example.com"
	kcPath    = "/apis/apps/v1/namespaces/team-a/deployments/web"
	kcMerge   = "application/merge-patch+json"
)

func kcKey(t *testing.T, ns, method, path, query, mt, body string) string {
	t.Helper()
	var b []byte
	if body != "" {
		canon, err := KubeCallBody(mt, []byte(body))
		if err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		b = canon
	}
	k, err := KubeCallKey(kcCluster, ns, method, path, query, mt, b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func restartAt(at string) string {
	return `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"` + at + `"}}}}}`
}

// A restart keys on what it restarts, not on when kubectl was run.
func TestKubeCallKeyStableAcrossRestartTimestamps(t *testing.T) {
	a := kcKey(t, "team-a", "PATCH", kcPath, "fieldManager=kubectl-rollout", KubeStrategicMergePatch, restartAt("2026-11-07T18:30:00Z"))
	b := kcKey(t, "team-a", "PATCH", kcPath, "fieldManager=kubectl-rollout", KubeStrategicMergePatch, restartAt("2026-11-07T19:45:10+01:00"))
	if a != b {
		t.Fatal("two runs of one restart have two keys")
	}
	// Anything beside the annotation is not a restart, so its time binds.
	extra := `{"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`
	x := kcKey(t, "team-a", "PATCH", kcPath, "", KubeStrategicMergePatch, strings.Replace(extra, "%s", "2026-11-07T18:30:00Z", 1))
	y := kcKey(t, "team-a", "PATCH", kcPath, "", KubeStrategicMergePatch, strings.Replace(extra, "%s", "2026-11-07T18:31:00Z", 1))
	if x == y {
		t.Fatal("a patch that also changes replicas lost its time from the key")
	}
	// The same body under another media type is not a restart either.
	if kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, restartAt("2026-11-07T18:30:00Z")) ==
		kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, restartAt("2026-11-07T18:31:00Z")) {
		t.Fatal("a merge patch of the annotation was keyed as a restart")
	}
}

// The key binds the namespace, resource, name and body.
func TestKubeCallKeyBindsNsResourceNameBody(t *testing.T) {
	base := kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, `{"spec":{"replicas":3}}`)
	for name, k := range map[string]string{
		"namespace": kcKey(t, "team-b", "PATCH", strings.Replace(kcPath, "team-a", "team-b", 1), "", kcMerge, `{"spec":{"replicas":3}}`),
		"resource":  kcKey(t, "team-a", "PATCH", strings.Replace(kcPath, "deployments", "statefulsets", 1), "", kcMerge, `{"spec":{"replicas":3}}`),
		"name":      kcKey(t, "team-a", "PATCH", kcPath+"x", "", kcMerge, `{"spec":{"replicas":3}}`),
		"body":      kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, `{"spec":{"replicas":4}}`),
		"method":    kcKey(t, "team-a", "PUT", kcPath, "", kcMerge, `{"spec":{"replicas":3}}`),
	} {
		if k == base {
			t.Errorf("another %s has the same key", name)
		}
	}
	if kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, "{ \"spec\" : {\"replicas\":3} }") != base {
		t.Error("the same body spelt another way has another key")
	}
}

// The key binds the canonical query and the parsed media type.
func TestKubeCallKeyBindsQueryAndMediaType(t *testing.T) {
	body := `{"spec":{"replicas":3}}`
	base := kcKey(t, "team-a", "PATCH", kcPath, "", kcMerge, body)
	for name, k := range map[string]string{
		"dry run":     kcKey(t, "team-a", "PATCH", kcPath, "dryRun=All", kcMerge, body),
		"media type":  kcKey(t, "team-a", "PATCH", kcPath, "", KubeStrategicMergePatch, body),
		"json patch":  kcKey(t, "team-a", "PATCH", kcPath, "", "application/json-patch+json", `[{"op":"add","path":"/x","value":1}]`),
		"field owner": kcKey(t, "team-a", "PATCH", kcPath, "fieldManager=other", kcMerge, body),
	} {
		if k == base {
			t.Errorf("another %s has the same key", name)
		}
	}
	a, err := KubeCanonicalQuery("labelSelector=app%3Dweb&dryRun=All")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := KubeCanonicalQuery("dryRun=All&labelSelector=app=web")
	if a != b || a != "dryRun=All&labelSelector=app%3Dweb" {
		t.Errorf("canonical queries %q and %q", a, b)
	}
	if c, _ := KubeCanonicalQuery("labelSelector=app%3Dapi"); c == a {
		t.Error("another selector has the same canonical query")
	}
	for _, bad := range []string{"a=%zz", "a=1;b=2"} {
		if _, err := KubeCanonicalQuery(bad); err == nil {
			t.Errorf("query %q accepted", bad)
		}
	}
}

// A body is JSON under a JSON media type, at most 32 KiB, with no repeated key.
func TestKubeCallBodyRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		mt, body string
		want     error
	}{
		"protobuf":     {"application/vnd.kubernetes.protobuf", "\x0a\x02", ErrKubeCallNotJSON},
		"yaml":         {"application/apply-patch+yaml", "a: 1", ErrKubeCallNotJSON},
		"no type":      {"", `{}`, ErrKubeCallNotJSON},
		"not JSON":     {"application/json", "a=1", ErrKubeCallNotJSON},
		"repeated key": {"application/json", `{"a":1,"a":2}`, ErrCanonicalRepeatedKey},
		"too large":    {"application/json", `"` + strings.Repeat("x", MaxMCPCallBytes) + `"`, ErrKubeCallTooLarge},
		"grows":        {"application/json", `"` + strings.Repeat("<", MaxMCPCallBytes/4) + `"`, ErrKubeCallTooLarge},
	} {
		if _, err := KubeCallBody(c.mt, []byte(c.body)); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
	if b, err := KubeCallBody("", nil); err != nil || b != nil {
		t.Errorf("no body: %q %v", b, err)
	}
}

func kcScope(t *testing.T) KubeCallScope {
	t.Helper()
	s := KubeCallScope{Cluster: kcCluster, Namespace: "team-a", Method: "PATCH", Path: kcPath, Query: "dryRun=All",
		MediaType: kcMerge, Body: json.RawMessage(`{"spec":{"replicas":3}}`)}
	k, err := s.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.CallSHA256 = k
	return s
}

// A scope a person could not read exactly does not validate.
func TestKubeCallScopeValidate(t *testing.T) {
	if err := kcScope(t).Validate(); err != nil {
		t.Fatalf("a well-formed scope: %v", err)
	}
	for name, mutate := range map[string]func(*KubeCallScope){
		"no cluster":          func(s *KubeCallScope) { s.Cluster = "" },
		"a GET":               func(s *KubeCallScope) { s.Method = "GET" },
		"namespace not path":  func(s *KubeCallScope) { s.Namespace = "team-b" },
		"cluster-wide path":   func(s *KubeCallScope) { s.Path = "/api/v1/nodes/n1" },
		"query in path":       func(s *KubeCallScope) { s.Path += "?x=1" },
		"query not canonical": func(s *KubeCallScope) { s.Query = "b=1&a=2" },
		"body not canonical":  func(s *KubeCallScope) { s.Body = json.RawMessage(`{"spec": {"replicas":3}}`) },
		"body not JSON type":  func(s *KubeCallScope) { s.MediaType = "application/yaml" },
		"body with no type":   func(s *KubeCallScope) { s.MediaType = "" },
	} {
		s := kcScope(t)
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// A kubectl call scope is never read as an MCP call scope, and the reverse.
func TestKubeCallScopeIsNotMCPCallScope(t *testing.T) {
	kube, _ := json.Marshal(kcScope(t))
	if !IsKubeCallScope(kube) || IsMCPCallScope(kube) {
		t.Fatalf("kube scope: kube %v mcp %v", IsKubeCallScope(kube), IsMCPCallScope(kube))
	}
	mcp := json.RawMessage(`{"tool":"mcp__jira__x","mcp_server":"jira","arguments":{},"call_sha256":"ab"}`)
	if IsKubeCallScope(mcp) || !IsMCPCallScope(mcp) {
		t.Fatal("an MCP scope was read as a kubectl one")
	}
	if MCPCallTextOf(kube) != nil {
		t.Fatal("a kubectl scope got MCP call text")
	}
}

// approve_each holds writes, so a read row cannot carry it.
func TestCLIKubectlApproveEachNeedsAWrite(t *testing.T) {
	for access, ok := range map[string]bool{ComponentAccessRead: false, ComponentAccessRestart: true, ComponentAccessWrite: true} {
		var k CLIKubectl
		if err := json.Unmarshal([]byte(`{"server":"https://k8s.example.com:6443","service_account":{"namespace":"ops","name":"runner"},`+
			`"namespaces":["team-a"],"access":"`+access+`","approve_each":true}`), &k); err != nil {
			t.Fatal(err)
		}
		if err := k.validate(); (err == nil) != ok {
			t.Errorf("access %s with approve_each: %v", access, err)
		}
	}
}
