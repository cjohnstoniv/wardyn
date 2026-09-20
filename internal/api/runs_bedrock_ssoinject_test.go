// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Phase-B (0.7.6) pins on the three runs_bedrock.go derivations the proxy
// injection depends on: the portal host the grant is bound to, the egress list
// that decides whether the transport may carry a credential at all, and the
// sandbox cache file that must stop carrying the real token.

func TestSSOPortalHost_BareHostFromRegionOrOverride(t *testing.T) {
	if got, want := ssoPortalHost("eu-west-2", ""), "portal.sso.eu-west-2.amazonaws.com"; got != want {
		t.Errorf("ssoPortalHost(region) = %q, want %q", got, want)
	}
	// BARE, never host:port: buildInjector keys byHost on the rule host verbatim
	// and both lanes resolve with the bare host.
	if got := ssoPortalHost("eu-west-2", theOverride); got != theOverrideHost {
		t.Errorf("ssoPortalHost(override) = %q, want the override's bare hostname %q", got, theOverrideHost)
	}
	if strings.Contains(ssoPortalHost("eu-west-2", theOverride), ":") {
		t.Error("ssoPortalHost returned a port-qualified host; the injection grant's scope.host must stay BARE")
	}
}

// The egress list must carry the port-QUALIFIED entry beside the bare one
// whenever the override names a port, or Policy.AuthoredPortFor answers false
// and injectableTransport withholds the credential on the cleartext fake lane —
// a walk that 401s with nothing in the proxy log to say why.
func TestSSOEgressHosts_OverrideWithPortCarriesBothEntries(t *testing.T) {
	got := ssoEgressHosts("eu-west-2", theOverride)
	want := []string{theOverrideHost, net.JoinHostPort(theOverrideHost, "8090")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ssoEgressHosts with a ported override = %v, want %v", got, want)
	}
	// A port-less override stays exactly one entry.
	if got := ssoEgressHosts("eu-west-2", "http://wardyn-awsssofake"); !reflect.DeepEqual(got, []string{"wardyn-awsssofake"}) {
		t.Errorf("ssoEgressHosts with a port-less override = %v, want one bare entry", got)
	}
}

func testSSOBlob() awsSSOBlob {
	return awsSSOBlob{
		AccessToken: "real-sso-access-token-value", RefreshToken: "real-refresh",
		ClientID: "cid", ClientSecret: "csec",
		StartURL: "https://example.awsapps.com/start", Region: "eu-west-2",
		AccountID: "111122223333", RoleName: "WardynAgent",
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
}

// proxyInjected=false is the 0.7.5 GOLDEN: byte-identical to what the one-arg
// function produced, so the kill switch's `off` position is a real rollback.
func TestAWSSSOCacheFileContents_ProxyInjectedOffIsTheOldBytes(t *testing.T) {
	b := testSSOBlob()
	// A FIXED expiry, so the golden can be BYTE-exact (general N5). The first
	// shape asserted a prefix and a field, which would have passed through a
	// reordered key, a dropped field or an added one — precisely the drift a
	// golden exists to catch on the switch's `off` position, whose whole promise
	// is "0.7.5 byte for byte".
	b.ExpiresAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	const want = `{"accessToken":"real-sso-access-token-value","expiresAt":"2030-01-02T03:04:05Z",` +
		`"region":"eu-west-2","startUrl":"https://example.awsapps.com/start"}`
	if got := awsSSOCacheFileContents(b, false); got != want {
		t.Errorf("the off path wrote\n  %s\nwant (0.7.5, byte for byte)\n  %s", got, want)
	}
	// The registration fields still ride a blob with NO refresh token, exactly
	// as 0.7.5 wrote them — the other half of the off path's bytes.
	withReg := b
	withReg.RefreshToken = ""
	withReg.ClientID, withReg.ClientSecret = "cid", "csec"
	const wantReg = `{"accessToken":"real-sso-access-token-value","clientId":"cid","clientSecret":"csec",` +
		`"expiresAt":"2030-01-02T03:04:05Z","region":"eu-west-2","startUrl":"https://example.awsapps.com/start"}`
	if got := awsSSOCacheFileContents(withReg, false); got != wantReg {
		t.Errorf("the off path with a registration wrote\n  %s\nwant\n  %s", got, wantReg)
	}
}

// proxyInjected=true: the real token is GONE from the sandbox and the file
// outlives any run (the SDK validates expiry locally and refreshes inside 5 min
// of it; a short expiry would make the sandbox try to refresh a placeholder).
func TestAWSSSOCacheFileContents_ProxyInjectedHoldsAPlaceholderThatOutlivesTheRun(t *testing.T) {
	b := testSSOBlob()
	got := awsSSOCacheFileContents(b, true)
	if strings.Contains(got, b.AccessToken) {
		t.Fatal("the REAL access token is in the proxy-injected sandbox cache file — Phase B's whole point is that it is not")
	}
	if strings.Contains(got, b.RefreshToken) || strings.Contains(got, b.ClientSecret) {
		t.Fatal("a refresh token or client secret reached the proxy-injected sandbox cache file")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("cache is not JSON: %v", err)
	}
	if m["accessToken"] != awsSSOPlaceholderToken {
		t.Errorf("accessToken = %v, want the inert placeholder %q", m["accessToken"], awsSSOPlaceholderToken)
	}
	exp, err := time.Parse(time.RFC3339, m["expiresAt"].(string))
	if err != nil {
		t.Fatalf("expiresAt is not RFC3339: %v", err)
	}
	if until := time.Until(exp); until < 7*24*time.Hour {
		t.Errorf("placeholder expiresAt is %v away, want > 7 days — the SDK attempts its own refresh inside 5 minutes of expiry", until)
	}
	// startUrl/region are operator configuration, not credentials: the SDK still
	// needs them to resolve the profile.
	if m["startUrl"] != b.StartURL || m["region"] != b.Region {
		t.Errorf("the proxy-injected cache lost the session identity: %v", m)
	}
}
