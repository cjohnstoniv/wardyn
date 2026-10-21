// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// netHintCause is the decision row cause a run's proxy writes for a forge host
// that resolved into CGNAT (internalHostsLiftHint, internal/egress/proxy).
const netHintCause = "an operator may lift this guard for dev.azure.com with site_config.internal_hosts, scoped to 100.64.0.0/10"

func egressDenyRow(age time.Duration, source, cause string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{"host": "dev.azure.com", "port": 443, "rule_source": source, "cause": cause})
	return types.AuditEvent{
		ID: uuid.New(), Time: refusalNow.Add(-age), ActorType: types.ActorAgent, Actor: "spiffe://wardyn/run/x",
		Action: "egress.deny", Target: "dev.azure.com", Outcome: "failure", Data: data,
	}
}

// TestPrivateIPRefusalsRow: a recent refusal whose cause names the lift is a warn
// row quoting it once however many lanes refused; a refusal with no lift, another
// rule source, or one older than the window is not; a declaration covering the
// block clears the row and one scoped to another block does not.
func TestPrivateIPRefusalsRow(t *testing.T) {
	st := &refusalStore{events: []types.AuditEvent{
		egressDenyRow(time.Hour, "builtin:private-ip", netHintCause),
		egressDenyRow(2*time.Hour, "brokered:git-pat:denied", netHintCause),
		egressDenyRow(3*time.Hour, "builtin:private-ip", ""),
		egressDenyRow(4*time.Hour, "policy:default-deny", "an operator may lift this guard for other.example with site_config.internal_hosts, scoped to 10.0.0.0/8"),
		egressDenyRow(8*24*time.Hour, "builtin:private-ip", "an operator may lift this guard for old.example with site_config.internal_hosts, scoped to 10.0.0.0/8"),
	}}
	srv := refusalServer(t, st)
	ctx := context.Background()

	chk, ok := srv.privateIPRefusalsRow(ctx, types.SiteConfig{})
	if !ok || chk.Status != "warn" || chk.Blocking || strings.Count(chk.Detail, netHintCause) != 1 ||
		strings.Contains(chk.Detail, "other.example") || strings.Contains(chk.Detail, "old.example") {
		t.Fatalf("row = %+v (present %v), want one warn quoting %q once", chk, ok, netHintCause)
	}
	assertSetupCheckBlocking(t, chk)

	other := types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "dev.azure.com", CIDRs: []string{"10.0.0.0/8"}}}}
	if _, ok := srv.privateIPRefusalsRow(ctx, other); !ok {
		t.Fatal("a declaration scoped to another block cleared the row")
	}
	for _, h := range []types.InternalHost{{HostSuffix: "dev.azure.com", CIDRs: []string{"100.64.0.0/10"}}, {HostSuffix: "azure.com"}} {
		if chk, ok := srv.privateIPRefusalsRow(ctx, types.SiteConfig{InternalHosts: []types.InternalHost{h}}); ok {
			t.Fatalf("declared %+v, row still present: %+v", h, chk)
		}
	}
	// The read is cached for privateIPRefusalTTL; past it the store is read again.
	st.events = st.events[2:]
	if _, ok := srv.privateIPRefusalsRow(ctx, types.SiteConfig{}); !ok {
		t.Fatal("the row was re-read inside the cache window")
	}
	srv.cfg.Now = func() time.Time { return refusalNow.Add(privateIPRefusalTTL) }
	if chk, ok := srv.privateIPRefusalsRow(ctx, types.SiteConfig{}); ok {
		t.Fatalf("no liftable refusal, row present: %+v", chk)
	}
}

// The row finds the proxy's refusals by the sentence the proxy writes; this holds
// the two together.
func TestPrivateIPLiftMarkerIsTheProxysSentence(t *testing.T) {
	src, err := os.ReadFile("../egress/proxy/egress_target.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"an operator may lift this guard for " + host + "`+privateIPLiftMarker+`" + block.String()`) {
		t.Fatalf("internal/egress/proxy internalHostsLiftHint no longer writes %q before the block", privateIPLiftMarker)
	}
}
