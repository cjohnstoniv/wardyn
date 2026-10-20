// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure CLI door: a run that attaches an Azure CLI row reaches Azure
// Resource Manager through this gate and nowhere else. The sandbox holds a
// profile and a token cache whose access token is an inert sentinel; every
// request to management.azure.com is judged by internal/armscope and forwarded
// with the person's own Resource Manager token, which the control plane redeems
// and this proxy alone holds.
//
// THIS IS THE BOUNDARY: that token carries the person's whole Azure role
// assignment, in every subscription they can reach, so the subscription and
// method check on the request line is the only thing that holds a run to its
// row. It runs on both lanes: the terminated tunnel judges each request, and
// the plain forward lane and a tunnel on any other port refuse every request to
// the host, so an absolute-form request line cannot reach it ungated.
//
// The door also owns login.microsoftonline.com for the run. The CLI's token
// library fetches the tenant's public OpenID configuration document every time
// it starts, before it reads its cache, so the proxy answers that one GET from
// a canned copy and never dials Entra. Every other path on the host (a token
// request, instance discovery, a device code) is refused: the sandbox can
// neither obtain nor renew a token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	ruleSourceAzureARM          = "brokered:azure-arm"
	ruleSourceAzureARMRefused   = "brokered:azure-arm:refused"
	ruleSourceAzureARMDiscovery = "brokered:azure-arm:tenant-discovery"

	// The `wardyn` codes of the door's two kinds of refusal.
	azureARMRefused               = "azure_arm_refused"
	azureARMCredentialUnavailable = "azure_arm_credential_unavailable"

	// azureARMPort is the one port the door terminates, on both of its hosts.
	azureARMPort = 443

	// AzureLoginHost is the Entra host the door answers the tenant's OpenID
	// configuration on and refuses everything else on.
	AzureLoginHost = "login.microsoftonline.com"
)

// What a sandbox reads when its Azure request is refused. Always 403, never
// 401, which the CLI reads as "sign in again" and answers with a token request.
const (
	azureARMRefusedPrefix    = "Wardyn refused this Azure request: "
	azureARMLoginRefusal     = "Wardyn refused this Microsoft Entra request: this run's Azure CLI is signed in by Wardyn, which adds the token to each Azure Resource Manager request, so the sandbox cannot sign in, renew a token or ask for another one."
	azureARMNoCredential     = azureARMRefusedPrefix + "this run has no Azure credential to add to it."
	azureARMCredentialFailed = "Wardyn could not renew this run's Azure sign-in."
	azureARMSignInTimedOut   = "Wardyn held this Azure request while its owner was asked to sign in to Azure again, " +
		"and nobody signed in before the hold expired. Nothing was substituted; retry once they have signed in."
	azureARMSignInEnded = "Wardyn asked this Azure request's owner to sign in to Azure again, and the request " +
		"ended before a sign-in arrived. Nothing was substituted."
)

// AzureARMGrant is the run's Azure CLI grant: the tenant the person signed in
// to, the subscriptions the row pins (each as armscope.Subscription spells it)
// and whether the row lets the CLI change them.
type AzureARMGrant struct {
	TenantID      string   `json:"tenant_id"`
	Subscriptions []string `json:"subscriptions"`
	Write         bool     `json:"write,omitempty"`
}

// azureARMDoor is the compiled grant.
type azureARMDoor struct {
	scope armscope.Scope
	// discoveryPath is the one path the door answers on the Entra host, and
	// discovery the document it answers with.
	discoveryPath string
	discovery     []byte
}

// newAzureARMDoor compiles the grant and adds the door's two hosts to the
// MITM-eligible set on its one port. Nil without a grant.
func newAzureARMDoor(g *AzureARMGrant, mitmHosts map[string]bool, mitmPorts map[string]int) *azureARMDoor {
	if g == nil {
		return nil
	}
	for _, h := range []string{armscope.Host, AzureLoginHost} {
		mitmHosts[h], mitmPorts[h] = true, azureARMPort
	}
	base := "https://" + AzureLoginHost + "/" + g.TenantID
	doc, _ := json.Marshal(map[string]string{
		"issuer":                        base + "/v2.0",
		"authorization_endpoint":        base + "/oauth2/v2.0/authorize",
		"token_endpoint":                base + "/oauth2/v2.0/token",
		"device_authorization_endpoint": base + "/oauth2/v2.0/devicecode",
	})
	return &azureARMDoor{
		scope:         armscope.Scope{Subscriptions: slices.Clone(g.Subscriptions), Write: g.Write},
		discoveryPath: "/" + g.TenantID + "/v2.0/.well-known/openid-configuration",
		discovery:     doc,
	}
}

// isAzureARMHost reports whether host is one of the door's two hosts on a run
// that has one.
func (p *Proxy) isAzureARMHost(host string) bool {
	h := patAPIHost(host)
	return p.arm != nil && (h == armscope.Host || h == AzureLoginHost)
}

// serveAzureARM is the door: judge, resolve, forward. The Entra host is
// answered here and never forwarded.
func (p *Proxy) serveAzureARM(w http.ResponseWriter, r *http.Request, host string, port int) {
	if patAPIHost(host) == AzureLoginHost {
		p.serveAzureLogin(w, r, host, port)
		return
	}
	if err := p.arm.scope.Admit(r.Method, adoRawPath(r), r.Header); err != nil {
		p.refuseAzureARM(w, r, host, port, azureARMRefused, azureARMRefusedPrefix+err.Error()+".")
		return
	}
	// After the gate, so a refused request never renews the token. The caller's
	// own context: a renewal that needs the person parks this request
	// (credhold.go), and a client that hangs up must release it.
	hdr, ok, err := p.inject.resolveCtx(r.Context(), host)
	switch {
	case err != nil && r.Context().Err() != nil:
		return // the client hung up; nothing was refused
	case err != nil:
		p.refuseAzureARMCredential(w, r, host, port, err)
		return
	case !ok:
		p.refuseAzureARM(w, r, host, port, azureARMCredentialUnavailable, azureARMNoCredential)
		return
	}
	rearmBodyDeadline(w) // the resolve may have held the request
	p.forwardAPIDoor(w, r, host, port, apiDoor{allow: ruleSourceAzureARM, denied: ruleSourceAzureARMRefused,
		mintFailed: "azure credential", upstream: "azure",
		credential: func(context.Context) (string, string, error) { return hdr.name, hdr.value, nil }})
}

// serveAzureLogin answers the run tenant's OpenID configuration from the canned
// copy and refuses every other request to the Entra host.
func (p *Proxy) serveAzureLogin(w http.ResponseWriter, r *http.Request, host string, port int) {
	if r.Method != http.MethodGet || !strings.EqualFold(adoRawPath(r), p.arm.discoveryPath) {
		p.refuseAzureARM(w, r, host, port, azureARMRefused, azureARMLoginRefusal)
		return
	}
	p.emitPATAPIDecision(r, host, port, egress.Allow, ruleSourceAzureARMDiscovery)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(p.arm.discovery)
}

// refuseAzureARM is the door's one refusal point: a decision row under
// brokered:azure-arm:refused and a 403 whose JSON carries the code and the
// sentence, in the error shape of the host it answers for, so the CLI prints
// the sentence.
func (p *Proxy) refuseAzureARM(w http.ResponseWriter, r *http.Request, host string, port int, code, message string) {
	p.emitPATAPIDecision(r, host, port, egress.Deny, ruleSourceAzureARMRefused)
	body := map[string]any{"wardyn": code, "error": map[string]string{"code": code, "message": message}}
	if patAPIHost(host) == AzureLoginHost {
		body = map[string]any{"wardyn": code, "error": "access_denied", "error_description": message}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(body)
}

// refuseAzureARMCredential answers a request the gate admitted whose token
// could not be resolved: a sign-in hold that ended without a sign-in, or any
// other failure of the renewal. A control-plane refusal keeps its own sentence.
func (p *Proxy) refuseAzureARMCredential(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	msg := adoControlPlaneRefusal(err, azureARMCredentialFailed)
	switch {
	case errors.Is(err, errReauthTimedOut):
		msg = azureARMSignInTimedOut
	case errors.Is(err, errReauthNoCredential):
		msg = azureARMSignInEnded
	}
	slog.Warn("proxy: an Azure CLI credential could not be resolved", "host", host, "err", reauthHoldError(err))
	p.refuseAzureARM(w, r, host, port, azureARMCredentialUnavailable, msg)
}

// refuseAzureARMPlain refuses, on the plain forward lane, every request to
// either of the door's hosts, and reports whether it did. That lane never runs
// the door.
func (p *Proxy) refuseAzureARMPlain(w http.ResponseWriter, r *http.Request) bool {
	if p.arm == nil || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if !p.isAzureARMHost(host) {
		return false
	}
	p.refuseAzureARM(w, r, host, port, azureARMRefused, azureARMRefusedPrefix+
		"this host is reached only through Wardyn's checked HTTPS tunnel (CONNECT), not as a plain proxy request.")
	return true
}

// refuseAzureARMTunnel refuses a CONNECT to either of the door's hosts that the
// proxy will not terminate (another port): a blind tunnel would carry it past
// the gate.
func (p *Proxy) refuseAzureARMTunnel(w http.ResponseWriter, r *http.Request, host string, port int) bool {
	if !p.isAzureARMHost(host) {
		return false
	}
	p.refuseAzureARM(w, r, host, port, azureARMRefused, azureARMRefusedPrefix+
		"this run reaches this host only through Wardyn's checked HTTPS door on port 443.")
	return true
}

// azureTenantSafe reports whether tenant can be written into the paths and the
// document the door serves: Entra's tenant identifiers are GUIDs or DNS names.
func azureTenantSafe(tenant string) bool {
	return tenant != "" && len(tenant) <= 128 && !strings.ContainsFunc(tenant, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '.' && c != '_'
	})
}

// validateAzureARM refuses a config whose Azure CLI grant the proxy could not
// honour, at boot rather than as a token riding an ungated path: no MITM CA
// (the door runs only on a terminated connection), a tenant or a subscription
// that is not one, no subscription at all, or anything but exactly one TLS-only,
// unpinned injection rule on the Resource Manager host — the door is that
// rule's only gate, and without the rule it has no token to add. It also
// refuses the reverse: a rule that names a CLI sign-in with no door in front
// of it, or on any other host, which the plain forward lane would otherwise
// credential with nothing judging the request.
func (c *Config) validateAzureARM() error {
	g := c.AzureARM
	for i, inj := range c.Injection {
		if strings.HasPrefix(inj.SecretName, types.CLISecretPrefix) && !types.CLIKubectlTokenSecret(inj.SecretName) && (g == nil || patAPIHost(strings.TrimSpace(inj.Host)) != armscope.Host) {
			return fmt.Errorf("config: injection[%d]: a CLI sign-in is injected only on %s behind azure_arm", i, armscope.Host)
		}
	}
	if g == nil {
		return nil
	}
	if c.MITMCACertPEM == "" || c.MITMCAKeyPEM == "" {
		return fmt.Errorf("config: azure_arm requires mitm_ca_cert_pem and mitm_ca_key_pem — the Azure CLI door runs only on a terminated connection")
	}
	if !azureTenantSafe(g.TenantID) {
		return fmt.Errorf("config: azure_arm.tenant_id %q is not a tenant identifier", g.TenantID)
	}
	if len(g.Subscriptions) == 0 {
		return fmt.Errorf("config: azure_arm.subscriptions must name at least one subscription")
	}
	for i, sub := range g.Subscriptions {
		if id, ok := armscope.Subscription(sub); !ok || id != sub || slices.Contains(g.Subscriptions[:i], sub) {
			return fmt.Errorf("config: azure_arm.subscriptions[%d]: %q is not a lowercase subscription GUID listed once", i, sub)
		}
	}
	rules := 0
	for i, inj := range c.Injection {
		switch patAPIHost(strings.TrimSpace(inj.Host)) {
		case AzureLoginHost:
			return fmt.Errorf("config: injection[%d]: no credential may be bound to %s on a run with azure_arm", i, AzureLoginHost)
		case armscope.Host:
			if !inj.RequireTLS || len(inj.PinRoutes) > 0 || inj.PinPath != "" {
				return fmt.Errorf("config: injection[%d]: the rule on %s must set require_tls and no pin — the Azure CLI door is its gate", i, armscope.Host)
			}
			rules++
		}
	}
	if rules != 1 {
		return fmt.Errorf("config: azure_arm requires exactly one injection rule on %s, found %d", armscope.Host, rules)
	}
	return nil
}
