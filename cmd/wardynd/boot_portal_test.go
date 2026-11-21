// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// The label shapes the activation reads (internal/store/store_portal_activation.go): restated here so a
// drift in either side fails a test, with the grammar of internal/version.Version.
var (
	testServerLabel = regexp.MustCompile(`^wardynd/(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	testToolLabel   = regexp.MustCompile(`^wardynd-[a-z0-9][a-z0-9-]*/(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$`)
)

func TestServerDBLabel_ShapesTheActivationReads(t *testing.T) {
	if l := serverDBLabel(); !testServerLabel.MatchString(l) || len(l) > dbLabelMax {
		t.Errorf("server label %q is not wardynd/<semver>/<replica> within %d bytes", l, dbLabelMax)
	}
	for _, tool := range []string{"rewrap", "rekey", "migrate-secrets", "migrate-only", "recovery-target", "rotate-gateway-identity-key", "rewrap-principal-keys"} {
		if l := toolDBLabel(tool); !testToolLabel.MatchString(l) || len(l) > dbLabelMax {
			t.Errorf("tool label %q is not wardynd-<tool>/<semver> within %d bytes", l, dbLabelMax)
		}
	}
	// A hostile or long host never breaks the shape or the cap, and the per-process suffix survives.
	for _, host := range []string{"", "pod/with slashes", strings.Repeat("a", 200), "---", "Ünïcode-host.example.com"} {
		id := buildReplicaID(host, "1a2b3c4d", "0.10.2-rc.1")
		l := "wardynd/0.10.2-rc.1/" + id
		if !testServerLabel.MatchString(l) || len(l) > dbLabelMax || !strings.HasSuffix(id, "1a2b3c4d") {
			t.Errorf("host %q gave label %q", host, l)
		}
	}
}

func TestPortalInternalIssuerHosts_ListAndCABundle(t *testing.T) {
	if got := portalInternalHostList(" idp.corp.example:8443 ,, idp2.corp.example ,"); len(got) != 2 || got[0] != "idp.corp.example:8443" || got[1] != "idp2.corp.example" {
		t.Errorf("host list = %q", got)
	}
	if got := portalInternalHostList(""); len(got) != 0 {
		t.Errorf("an empty setting lists %q, want none", got)
	}
	counter := &api.PortalRefreshFailures{}
	if _, err := portalIssuerOptions("idp.corp.example", filepath.Join(t.TempDir(), "missing.pem"), "ops-seat", counter); err == nil || !strings.Contains(err.Error(), "WARDYN_PORTAL_INTERNAL_ISSUER_CA_FILE") {
		t.Errorf("an unreadable CA file: %v, want a boot refusal naming the setting", err)
	}
	opts, err := portalIssuerOptions("idp.corp.example", "", "ops-seat", counter)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Reserved("OPS-SEAT") || !opts.Reserved("delegate:x") || opts.Reserved("someone@corp.example") {
		t.Error("the verifier's reserved check is not the console's (api.PortalReservedCheck)")
	}
	pem := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(pem, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := portalIssuerOptions("idp.corp.example", pem, "ops-seat", counter); err != nil {
		t.Fatalf("reading the bundle is the verifier's to judge, not the option builder's: %v", err)
	}
}

func TestPortalRegistrationsText_NamesBounded(t *testing.T) {
	regs := []portalReg{{id: "id-a", name: "alpha"}, {id: "id-b", name: "beta", revoked: true}}
	if got, want := portalRegistrationsText(2, regs), `portal registrations: id-a "alpha", id-b "beta" (revoked)`; got != want {
		t.Errorf("two portals on one issuer = %q, want %q", got, want)
	}
	if got := portalRegistrationsText(0, nil); got != "no portal registration names it" {
		t.Errorf("an orphaned binding = %q", got)
	}
	var many []portalReg
	for i := range portalRegistrationMax {
		many = append(many, portalReg{id: fmt.Sprintf("id-%d", i), name: fmt.Sprintf("p%d", i)})
	}
	if got := portalRegistrationsText(portalRegistrationMax+3, many); strings.Count(got, `"p`) != portalRegistrationMax || !strings.HasSuffix(got, " and 3 more") {
		t.Errorf("%d registrations = %q, want %d named and the rest counted", portalRegistrationMax+3, got, portalRegistrationMax)
	}
}
