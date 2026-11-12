// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// The runner wire carries a create's proxy config hash and proxy address, and a revive's hash,
// back to the organisation.
func TestReadback_WireCarriesTheHashAndAddress(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, spec := create(t, rig, nil)
	if sb.ProxyConfigSHA256 == "" || sb.ProxyConfigSHA256 != runnertest.FakeProxyConfigSHA256(spec) || sb.ProxyAddr != runnertest.FakeProxyAddr {
		t.Fatalf("create = %+v; want the runner's hash and proxy address", sb)
	}
	var rv runner.ProxyReviver = rig.Sub
	hashed, ok := rv.(runner.ProxyReviverHashed)
	if !ok {
		t.Fatal("the remote substrate is not a runner.ProxyReviverHashed")
	}
	cfg := []byte(`{"stripped":true}`)
	sum, err := hashed.ReplaceProxyHashed(ctxT(t), sb.Ref, cfg)
	if err != nil || sum != proxy.ConfigSHA256(cfg) {
		t.Fatalf("ReplaceProxyHashed = %q, %v; want %s", sum, err, proxy.ConfigSHA256(cfg))
	}
	if !rig.Fake.Called(`replace_proxy {"stripped":true}`, strings.TrimPrefix(sb.Ref, "runner:"+rid+"/")) {
		t.Fatal("the runner never saw the revive")
	}
}
