// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// A runner answers its substrate's proxy config hash and proxy address on create, and its revive
// hash on replace_proxy through the same durable ref-call hooks (the offline manager's) as before.
func TestReadback_RunnerAnswersTheHashOnCreateAndRevive(t *testing.T) {
	f := newResidentFixture(t, nil)
	var seen []string
	f.d.cfg.BeforeRefCall = func(_ context.Context, method, _ string, _ uuid.UUID) error {
		seen = append(seen, "before "+method)
		return nil
	}
	f.d.cfg.AfterRefCall = func(_ context.Context, method, _, _ string, _ uuid.UUID) error {
		seen = append(seen, "after "+method)
		return nil
	}
	spec := runner.SandboxSpec{RunID: f.run, RunnerID: f.id}
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	got, err := f.d.Call(context.Background(), f.peer, 3, runnerwire.MethodCreateSandbox, data)
	if err != nil {
		t.Fatal(err)
	}
	sb := got.(runnerwire.CreateSandboxResult).Sandbox
	if sb.ProxyConfigSHA256 != runnertest.FakeProxyConfigSHA256(f.fake.Created[0]) || sb.ProxyAddr != runnertest.FakeProxyAddr {
		t.Fatalf("create answered %+v; want the substrate's hash and proxy address", sb)
	}

	var rv runner.ProxyReviver = f.fake
	if _, ok := rv.(runner.ProxyReviverHashed); !ok {
		t.Fatal("the test substrate cannot hash a revive")
	}
	cfg := []byte(`{"run_id":"` + f.run.String() + `"}`)
	ref := placement.RunnerRefPrefix(f.id) + "fake-" + f.run.String()
	data, _ = runnerwire.Marshal(runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: cfg})
	got, err = f.d.Call(context.Background(), f.peer, 4, runnerwire.MethodReplaceProxy, data)
	if err != nil {
		t.Fatal(err)
	}
	if res, ok := got.(runnerwire.ReplaceProxyResult); !ok || res.ConfigSHA256 != proxy.ConfigSHA256(cfg) {
		t.Fatalf("replace_proxy answered %#v, want the hash of %s", got, cfg)
	}
	if len(seen) != 2 || seen[0] != "before replace_proxy" || seen[1] != "after replace_proxy" {
		t.Fatalf("ref-call hooks saw %v; the hashed revive must stay replace_proxy to them", seen)
	}
}
