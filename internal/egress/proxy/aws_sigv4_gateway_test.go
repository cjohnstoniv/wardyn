// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A Bedrock gateway on a SSO-signed run: the sandbox's SDK targets the gateway, the proxy signs
// each request as the person for the regional Bedrock host behind it, and sends it to the gateway
// with the run's identity token.
const (
	sgwHost     = "ai-gw.corp.example"
	sgwPrefix   = "/bedrock"
	sgwAudience = "https://ai-gw.corp.example/wardyn"
	sgwProvider = "prov-bedrock-sso"
	sgwControl  = "bedrock.us-east-1.amazonaws.com"
)

func sgwRoute() GatewayIdentityRoute {
	return GatewayIdentityRoute{Host: sgwHost, Port: 443, PathPrefix: sgwPrefix, ProviderID: sgwProvider, Audience: sgwAudience}
}

func sgwEntry() AWSSigV4Gateway {
	return AWSSigV4Gateway{Host: sgwHost, Port: 443, PathPrefix: sgwPrefix, SigningHost: sigv4Bedrock}
}

// sgwHarness is the re-signing harness with one gateway entry, its identity route, the Bedrock
// control host re-signed directly, and a mint that answers a fresh token for the route.
type sgwHarness struct {
	*sigv4Harness
	mu     sync.Mutex
	minted []string
}

func newSGWHarness(t *testing.T) *sgwHarness {
	t.Helper()
	g := &sgwHarness{}
	g.sigv4Harness = newSigV4HarnessWith(t, func(o *Options) {
		o.AWSSigV4.Hosts = append(slices.Clone(sigv4Hosts), sgwControl)
		o.AWSSigV4.Gateways = []AWSSigV4Gateway{sgwEntry()}
		o.GatewayIdentity = []GatewayIdentityRoute{sgwRoute()}
		o.ControlPlaneURL = "https://wardynd.test:8443"
		o.ControlTLS = testInsecureTLSConfig
	}, sgwHost+":443", sgwControl+":443")
	g.p.maskKey = uuid.New()
	t.Cleanup(func() { procRegistry.Evict(g.p.maskKey) })
	g.mint = func(w http.ResponseWriter, r *http.Request) {
		var ask struct {
			ProviderID string `json:"provider_id"`
		}
		if r.Header.Get("Authorization") != "Bearer RUNTOK" || json.NewDecoder(r.Body).Decode(&ask) != nil || ask.ProviderID != sgwProvider {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tok := gwidToken(sgwAudience, time.Now().Add(10*time.Minute), int64(g.mints.Load()))
		g.mu.Lock()
		g.minted = append(g.minted, tok)
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": time.Now().Add(10 * time.Minute)})
	}
	return g
}

func (g *sgwHarness) lastMinted(t *testing.T) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.minted) == 0 {
		t.Fatal("nothing was minted")
	}
	return g.minted[len(g.minted)-1]
}

// sgwRequest is a Converse call the sandbox's SDK signed with the placeholder for Bedrock in
// us-east-1, aimed at host.
func sgwRequest(t *testing.T, host, target, body string, hdr map[string]string) *http.Request {
	t.Helper()
	return sandboxRequest(t, http.MethodPost, host, target, body, hdr, "", "bedrock", "us-east-1", "")
}

// sgwSignedHeaders is the SignedHeaders list of an Authorization value.
func sgwSignedHeaders(auth string) []string {
	_, params, _ := strings.Cut(auth, " ")
	for f := range strings.SplitSeq(params, ",") {
		if k, v, _ := strings.Cut(strings.TrimSpace(f), "="); k == "SignedHeaders" {
			return strings.Split(v, ";")
		}
	}
	return nil
}

// sgwExpected is the signature AWS computes for the call at the regional host A: an independent
// signer over Host A, the path with the gateway's prefix cut, the request's content type, the
// role credentials and the arrival's own timestamp. Accept-Encoding is not in it: the proxy drops
// that header for the identity route before it signs.
func sgwExpected(t *testing.T, a sigv4Arrival, cut, body, contentType string) string {
	t.Helper()
	at, err := time.Parse("20060102T150405Z", a.header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := url.PathUnescape(cut)
	if err != nil {
		t.Fatal(err)
	}
	r := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: sigv4Bedrock, Path: path, RawPath: cut},
		Host: sigv4Bedrock, Header: http.Header{"Content-Type": {contentType}}, ContentLength: int64(len(body))}
	sum := sha256.Sum256([]byte(body))
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: sigv4RoleKey, SecretAccessKey: sigv4RoleSecret, SessionToken: sigv4RoleToken},
		r, hex.EncodeToString(sum[:]), "bedrock", "us-east-1", at); err != nil {
		t.Fatal(err)
	}
	return r.Header.Get("Authorization")
}

// The call reaches the gateway at its own host and path, signed for the regional Bedrock host
// with the gateway's prefix cut, so AWS verifies it once the gateway forwards it unchanged.
// REVERT PROOF: signing the shadow with out.Host (the gateway's host) fails here, naming the
// regional host.
func TestAWSSigV4Gateway_SignsForTheRegionalHost(t *testing.T) {
	for name, cut := range map[string]string{
		"model id":                    "/model/anthropic.claude-x:0/converse",
		"inference profile ARN (%2F)": "/model/arn:aws:bedrock:us-east-1:111122223333:inference-profile%2Fus.anthropic.claude-x:0/converse",
	} {
		t.Run(name, func(t *testing.T) {
			g := newSGWHarness(t)
			body := `{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`
			rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+cut, body,
				map[string]string{"Content-Type": "application/json", "Accept-Encoding": "gzip, deflate"}), sgwHost)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body)
			}
			seen := g.arrivals()
			if len(seen) != 1 {
				t.Fatalf("the gateway saw %d requests, want 1", len(seen))
			}
			a := seen[0]
			if a.host != sgwHost {
				t.Fatalf("the call went to %q, want the gateway %q", a.host, sgwHost)
			}
			if a.uri != sgwPrefix+cut {
				t.Fatalf("the gateway got path %q, want %q byte for byte", a.uri, sgwPrefix+cut)
			}
			signed := sgwSignedHeaders(a.header.Get("Authorization"))
			if !slices.Contains(signed, "content-type") || slices.Contains(signed, "accept-encoding") {
				t.Fatalf("SignedHeaders %v: want content-type signed and accept-encoding (which the route drops) not", signed)
			}
			if got, want := a.header.Get("Authorization"), sgwExpected(t, a, cut, body, "application/json"); got != want {
				t.Fatalf("the signature does not verify for the regional host %s over the cut path %s\n got %s\nwant %s", sigv4Bedrock, cut, got, want)
			}
			if a.header.Get("X-Amz-Security-Token") != sigv4RoleToken {
				t.Fatalf("X-Amz-Security-Token %q, want the role's session token", a.header.Get("X-Amz-Security-Token"))
			}
			if strings.Contains(rec.Body.String(), sigv4RoleSecret) || strings.Contains(g.log(), sigv4RoleSecret) {
				t.Fatal("the role secret reached the sandbox or the decision log")
			}
		})
	}
}

// The token goes to the gateway and nowhere else: the control host, re-signed directly, gets
// none, and the sandbox's own header and call id never pass on either.
func TestAWSSigV4Gateway_CarriesTheIdentity(t *testing.T) {
	g := newSGWHarness(t)
	forged := map[string]string{"Content-Type": "application/json", gatewayIdentityHeader: gwidForged, gatewayCallIDHeader: gwidCallID}
	rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, forged), sgwHost)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway call: status %d body %s, want 200", rec.Code, rec.Body)
	}
	rec = g.send(t, sgwRequest(t, sgwControl, "/foundation-models", "", forged), sgwControl)
	if rec.Code != http.StatusOK {
		t.Fatalf("control call: status %d body %s, want 200", rec.Code, rec.Body)
	}
	seen := g.arrivals()
	if len(seen) != 2 || seen[0].host != sgwHost || seen[1].host != sgwControl {
		t.Fatalf("arrivals %d, want the gateway then the control host", len(seen))
	}
	gw, control := seen[0], seen[1]
	if got := gw.header.Get(gatewayIdentityHeader); got != g.lastMinted(t) {
		t.Fatalf("the gateway got identity %q, want the minted token", got)
	}
	callID := gw.header.Get(gatewayCallIDHeader)
	if callID == "" || callID == gwidCallID {
		t.Fatalf("the gateway got call id %q, want a fresh one", callID)
	}
	if v := control.header.Values(gatewayIdentityHeader); len(v) != 0 {
		t.Fatalf("the control host got an identity header %q", v)
	}
	if v := control.header.Values(gatewayCallIDHeader); len(v) != 0 {
		t.Fatalf("the control host got a call id %q", v)
	}
	verifyArrival(t, control, "bedrock")
	log := g.log()
	if !strings.Contains(log, callID) {
		t.Fatalf("no decision row carries the call id the gateway got (%s)", callID)
	}
	if strings.Contains(log, gwidForged) {
		t.Fatal("the sandbox's own identity value reached the decision log")
	}
}

// A path the route does not cover is refused before anything is minted or sent: outside the
// prefix, across its segment boundary, or with a dot segment, literal or encoded.
func TestAWSSigV4Gateway_RefusesOutsideThePrefix(t *testing.T) {
	for _, target := range []string{
		"/other/model/anthropic.claude-x:0/converse",
		"/bedrockx/model/anthropic.claude-x:0/converse",
		"/bedrock/../admin/model/anthropic.claude-x:0/converse",
		"/bedrock/model/%2e%2e/%2E%2E/admin/converse",
	} {
		t.Run(target, func(t *testing.T) {
			g := newSGWHarness(t)
			rec := g.send(t, sgwRequest(t, sgwHost, target, `{}`, map[string]string{"Content-Type": "application/json"}), sgwHost)
			if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4GatewayPath {
				t.Fatalf("status %d reason %q, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), awsSigV4GatewayPath)
			}
			if n := len(g.arrivals()); n != 0 {
				t.Fatalf("the gateway saw %d requests, want none", n)
			}
			if n := g.mints.Load(); n != 0 {
				t.Fatalf("%d mints for a refused request, want none", n)
			}
		})
	}
}

// A path off the gateway's route is refused before a host rule can hold the call, so no person is
// asked about a call that is never sent. REVERT PROOF: without the AWS SigV4 branch in
// gatewayOffRouteRefusedInTunnel the hold asks first.
func TestAWSSigV4Gateway_OffPrefixRefusedBeforeAHoldAsks(t *testing.T) {
	g := newSGWHarness(t)
	g.p.policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: append(sigv4HostPorts(), sgwHost+":443", sgwControl+":443"),
		HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: sgwHost, Effect: types.HostRuleHold}}}}})
	cp := newHCCP(t, g.p)
	ct := map[string]string{"Content-Type": "application/json"}
	rec := g.send(t, sgwRequest(t, sgwHost, "/other/model/anthropic.claude-x:0/converse", `{}`, ct), sgwHost)
	if _, raises, _ := cp.counts(); rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4GatewayPath ||
		raises != 0 || len(g.arrivals()) != 0 || g.mints.Load() != 0 {
		t.Fatalf("status %d reason %q, %d holds raised, %d arrivals, %d mints; want 403 %s and none of them",
			rec.Code, rec.Header().Get("X-Wardyn-Reason"), raises, len(g.arrivals()), g.mints.Load(), awsSigV4GatewayPath)
	}
	g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, ct), sgwHost)
	if _, raises, _ := cp.counts(); raises == 0 {
		t.Error("a call on the route raised no hold, so the zero above proves nothing")
	}
}

// A request to the gateway on a port its entry does not name is refused with its own reason, naming
// the port, before anything is minted or sent.
func TestAWSSigV4Gateway_RefusesAnotherPort(t *testing.T) {
	g := newSGWHarness(t)
	rec := httptest.NewRecorder()
	req := sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, map[string]string{"Content-Type": "application/json"})
	g.p.serveAWSSigV4(rec, req, sgwHost, 8443)
	if rec.Code != http.StatusForbidden || rec.Header().Get("X-Wardyn-Reason") != awsSigV4GatewayPort {
		t.Fatalf("status %d reason %q, want 403 %s", rec.Code, rec.Header().Get("X-Wardyn-Reason"), awsSigV4GatewayPort)
	}
	if body := rec.Body.String(); !strings.Contains(body, "only on port 443") || strings.Contains(body, "dot segment") {
		t.Fatalf("body %q, want the port sentence", body)
	}
	if n := len(g.arrivals()); n != 0 {
		t.Fatalf("the gateway saw %d requests, want none", n)
	}
	if n := g.mints.Load(); n != 0 {
		t.Fatalf("%d mints for a refused request, want none", n)
	}
}

// The gateway's answer is always scrubbed: an echoed token comes back masked, in a header and
// in the body.
func TestAWSSigV4Gateway_ResponseScrubbed(t *testing.T) {
	g := newSGWHarness(t)
	g.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Header.Get(gatewayIdentityHeader))
		_, _ = w.Write([]byte(`{"echo":"` + r.Header.Get(gatewayIdentityHeader) + `"}`))
	}
	rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, map[string]string{"Content-Type": "application/json"}), sgwHost)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body)
	}
	token := g.lastMinted(t)
	gwidNotIn(t, "the body", rec.Body.String(), token)
	gwidNotIn(t, "X-Echo", rec.Header().Get("X-Echo"), token)
	gwidNotIn(t, "the body", rec.Body.String(), token[strings.LastIndexByte(token, '.')+1:][:gatewaySignatureHeadLen])
}

// A refused mint sends nothing: no person-signed request reaches the gateway without identity.
func TestAWSSigV4Gateway_NoTokenNothingSent(t *testing.T) {
	g := newSGWHarness(t)
	g.mint = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/anthropic.claude-x:0/converse", `{}`, map[string]string{"Content-Type": "application/json"}), sgwHost)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s, want 502", rec.Code, rec.Body)
	}
	if n := len(g.arrivals()); n != 0 {
		t.Fatalf("the gateway saw %d requests with no identity to send, want none", n)
	}
	if g.mints.Load() == 0 {
		t.Fatal("no mint was asked")
	}
}

// sgwAWSHosts are provider-owned host shapes a gateway can never be, including those the
// re-signer itself does not take (GovCloud PrivateLink, China, dual-stack, a customer's
// PrivateLink service).
var sgwAWSHosts = []string{
	sigv4Bedrock,
	"bedrock-runtime.us-gov-west-1.amazonaws.com",
	"vpce-0123456789abcdef0-lj2wisx3.bedrock-runtime.us-gov-west-1.vpce.amazonaws.com",
	"vpce-0123456789abcdef0-lj2wisx3.vpce-svc-0123456789abcdef0.us-east-1.vpce.amazonaws.com",
	"bedrock-runtime.cn-north-1.amazonaws.com.cn",
	"vpce-0123456789abcdef0-lj2wisx3.bedrock-runtime.cn-north-1.vpce.amazonaws.com.cn",
	"bedrock-runtime.us-east-1.api.aws",
	"s3.dualstack.us-east-1.amazonaws.com",
	"bedrock-runtime.cn-north-1.api.amazonwebservices.com.cn",
	"abc123.execute-api.us-east-1.amazonaws.com",
	"amazonaws.com",
	"api.aws",
	"amazonaws.com.cn",
}

func TestAWSProviderOwnedHost(t *testing.T) {
	for _, h := range sgwAWSHosts {
		for _, spelling := range []string{h, strings.ToUpper(h), h + ".", " " + h} {
			if !AWSProviderOwnedHost(spelling) {
				t.Errorf("AWSProviderOwnedHost(%q) = false, want true", spelling)
			}
		}
	}
	for _, h := range []string{sgwHost, "amazonaws.com.evil.example", "notamazonaws.com", "api.aws.corp.example", "myapi.aws", "amazonaws.co", ""} {
		if AWSProviderOwnedHost(h) {
			t.Errorf("AWSProviderOwnedHost(%q) = true, want false", h)
		}
	}
}

func sgwGoodConfig() *Config {
	return &Config{MITMCACertPEM: "cert", MITMCAKeyPEM: "key",
		AWSSigV4: &AWSSigV4Config{GrantID: uuid.New(), PortalHost: sigv4Portal, AccountID: sigv4Account, RoleName: sigv4Role,
			Hosts: []string{sigv4Bedrock, sgwControl}, Gateways: []AWSSigV4Gateway{sgwEntry()}},
		GatewayIdentity: []GatewayIdentityRoute{sgwRoute()},
	}
}

// Sealed-config validation refuses a gateway entry the proxy could not honour safely. A gateway
// on a provider-owned host is refused by its own predicate, whatever the re-signer takes, so no
// such config starts a proxy, and none of those shapes is ever compiled as a gateway.
func TestAWSSigV4Gateway_ConfigValidation(t *testing.T) {
	if err := sgwGoodConfig().validateTerminatedGates(); err != nil {
		t.Fatalf("good config refused: %v", err)
	}
	gw := func(c *Config) *AWSSigV4Gateway { return &c.AWSSigV4.Gateways[0] }
	muts := map[string]func(c *Config){
		"signing host is the control host":   func(c *Config) { gw(c).SigningHost = sgwControl },
		"signing host is S3":                 func(c *Config) { gw(c).SigningHost = sigv4S3 },
		"signing host is dual-stack":         func(c *Config) { gw(c).SigningHost = "bedrock-runtime.us-east-1.api.aws" },
		"signing host is FIPS":               func(c *Config) { gw(c).SigningHost = "bedrock-runtime-fips.us-east-1.amazonaws.com" },
		"signing host is PrivateLink":        func(c *Config) { gw(c).SigningHost = sigv4BedrockPL },
		"signing host is the gateway":        func(c *Config) { gw(c).SigningHost = sgwHost },
		"signing host uppercase":             func(c *Config) { gw(c).SigningHost = strings.ToUpper(sigv4Bedrock) },
		"no identity route":                  func(c *Config) { c.GatewayIdentity = nil },
		"route on another prefix":            func(c *Config) { c.GatewayIdentity[0].PathPrefix = "/other" },
		"route on another port":              func(c *Config) { c.GatewayIdentity[0].Port = 8443 },
		"route is an MCP server's":           func(c *Config) { c.GatewayIdentity[0].ProviderID, c.GatewayIdentity[0].MCPKey = "", "mcp-1" },
		"gateway also in hosts":              func(c *Config) { c.AWSSigV4.Hosts = append(c.AWSSigV4.Hosts, sgwHost) },
		"dot segment in prefix":              func(c *Config) { gw(c).PathPrefix, c.GatewayIdentity[0].PathPrefix = "/a/../bedrock", "/a/../bedrock" },
		"prefix not a path":                  func(c *Config) { gw(c).PathPrefix, c.GatewayIdentity[0].PathPrefix = "bedrock", "bedrock" },
		"port out of range":                  func(c *Config) { gw(c).Port, c.GatewayIdentity[0].Port = 0, 0 },
		"host uppercase":                     func(c *Config) { gw(c).Host = strings.ToUpper(sgwHost) },
		"host with a port":                   func(c *Config) { gw(c).Host = sgwHost + ":443" },
		"gateway listed twice":               func(c *Config) { c.AWSSigV4.Gateways = append(c.AWSSigV4.Gateways, sgwEntry()) },
		"injection rule on the gateway host": func(c *Config) { c.Injection = []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: sgwHost}}} },
		"mitm entry on the gateway host":     func(c *Config) { c.MITMHosts = []string{sgwHost + ":443"} },
	}
	for _, h := range sgwAWSHosts {
		muts["provider-owned gateway "+h] = func(c *Config) { gw(c).Host, c.GatewayIdentity[0].Host = h, h }
	}
	for name, mut := range muts {
		c := sgwGoodConfig()
		mut(c)
		if err := c.validateTerminatedGates(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A config with no host to re-sign directly but a gateway is one the proxy honours.
	c := sgwGoodConfig()
	c.AWSSigV4.Hosts = nil
	if err := c.validateTerminatedGates(); err != nil {
		t.Fatalf("gateway-only config refused: %v", err)
	}
	// Built without validation, the door still never treats a provider-owned host as a gateway.
	for _, h := range sgwAWSHosts {
		d := newAWSSigV4Door(&AWSSigV4Config{Gateways: []AWSSigV4Gateway{{Host: h, Port: 443, PathPrefix: sgwPrefix, SigningHost: sigv4Bedrock}}},
			map[string]bool{}, map[string]int{})
		if sc, ok := d.scopes[hostKey(h)]; ok && sc.signHost != "" {
			t.Errorf("%s was compiled as a gateway", h)
		}
	}
}

// An older proxy, whose aws_sigv4 has no gateways, refuses a config that carries one, so the
// run fails at start; a grant without gateways renders exactly as it did before.
func TestAWSSigV4Gateway_OlderProxyRefusesGateways(t *testing.T) {
	type previousAWSSigV4Config struct {
		GrantID    uuid.UUID `json:"grant_id"`
		PortalHost string    `json:"portal_host"`
		AccountID  string    `json:"account_id"`
		RoleName   string    `json:"role_name"`
		Hosts      []string  `json:"hosts"`
	}
	g := sgwGoodConfig().AWSSigV4
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var old previousAWSSigV4Config
	if err := dec.Decode(&old); err == nil || !strings.Contains(err.Error(), "gateways") {
		t.Fatalf("an older proxy decoded a grant with gateways: err %v", err)
	}
	g.Gateways = nil
	now, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(previousAWSSigV4Config{GrantID: g.GrantID, PortalHost: g.PortalHost, AccountID: g.AccountID, RoleName: g.RoleName, Hosts: g.Hosts})
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(before) {
		t.Fatalf("a grant without gateways changed on the wire:\n now %s\nwant %s", now, before)
	}
}
