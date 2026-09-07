// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestOwnSubnetExclusionFailsClosedWhenTheStartupCaptureFails pins F002: the
// own-subnet/control-plane exclusion is a CLAMP on the two admin-authored
// exceptions to the private-IP guard (the InternalHosts lift and the
// exact-literal-IP redirect trust), so when its startup capture fails, the
// exceptions must be REFUSED — not silently permitted.
//
// NewServer captures both inputs best effort. localInterfaceSubnets() returned
// nil on error and resolveControlPlaneIP() returned nil on any parse/resolve
// failure, and with either nil the clamp evaluated to false, which makes the
// exceptions fire MORE widely rather than less. NewServer's own comment claimed
// the opposite ("fail closed toward the ORIGINAL unconditional deny, not toward
// widening it") and nothing logged the nil. On Kubernetes the pod's own
// interface does not carry the wardynd ClusterIP, so there the control-plane
// answers are the ONLY thing between a declared internal host and the control
// plane.
//
// Driven through NewServer rather than newProxy on purpose: the failure this
// pins is in the production startup capture, not in a hand-built Options.
func TestOwnSubnetExclusionFailsClosedWhenTheStartupCaptureFails(t *testing.T) {
	cfg := &Config{
		RunID: uuid.New(),
		// A control-plane host that cannot resolve: exactly the "lookup failed"
		// case the comment describes. ".invalid" is reserved by RFC 2606 and
		// never resolves.
		ControlPlaneURL: "http://wardynd.this-host-does-not-exist.invalid:8080",
		RunToken:        "tok",
		Policy: types.RunPolicySpec{
			AllowedDomains: []string{"registry.corp.internal"},
		},
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}},
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: 2 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	p := srv.proxy
	p.res = fakeResolver{m: map[string][]net.IP{"registry.corp.internal": ips("10.40.1.5")}}

	if guard := p.vetHost("registry.corp.internal"); !guard.Denied {
		t.Fatalf("the internal-host lift FIRED (ip=%v lifted=%v) although the startup capture of the "+
			"own-subnet/control-plane exclusion failed: an unavailable clamp must refuse the exception, "+
			"not widen it — on k8s the resolved control-plane address is the only thing this clamp has",
			guard.IP, guard.Lifted)
	}
	if p.trustsExactLiteralIP(net.ParseIP("10.40.1.5"), 443) {
		t.Fatal("the exact-literal-IP redirect trust fired under the same failed capture")
	}
	if !p.onOwnSubnetOrControlPlane(net.ParseIP("10.40.1.5")) {
		t.Fatal("with the capture failed the clamp must answer yes for every address")
	}
}
