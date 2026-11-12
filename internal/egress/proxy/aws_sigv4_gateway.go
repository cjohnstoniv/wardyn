// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
)

// A Bedrock gateway on a re-signed run (aws_sigv4.gateways). The sandbox's SDK targets the
// gateway and signs with the placeholder. The proxy signs each request again as the person, for
// the regional Bedrock host behind the gateway with the gateway's path prefix cut, and sends it
// to the gateway unchanged otherwise, with the run's identity token. The gateway checks the token,
// strips X-Wardyn-Identity and X-Wardyn-Call-Id, cuts its prefix and forwards the call to that host as it is, so
// AWS verifies the person's own signature. Off unless the control plane writes an entry, which it
// does only for a provider whose admin turned run identity on: the person's AWS identity then
// reaches AWS through the gateway, and the gateway sees each signed call but never the secret key.

// awsSigV4GatewayPath is the refusal of a gateway request the route's prefix does not cover, and
// awsSigV4GatewayPort of one on a port other than the entry's.
const (
	awsSigV4GatewayPath = "aws_sigv4_gateway_path"
	awsSigV4GatewayPort = "aws_sigv4_gateway_port"
)

// AWSSigV4Gateway is one gateway the run's Bedrock calls go to, signed for SigningHost. The run's
// gateway_identity carries a provider route with the same Host, Port and PathPrefix.
type AWSSigV4Gateway struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// PathPrefix is the gateway's base path, "" for the whole host; it is cut before signing.
	PathPrefix string `json:"path_prefix,omitempty"`
	// SigningHost is bedrock-runtime.<region>.amazonaws.com, the host AWS verifies the call for.
	SigningHost string `json:"signing_host"`
}

// awsProviderDomains are the DNS zones AWS serves its own endpoints under: the commercial and
// GovCloud partitions and their PrivateLink names (amazonaws.com), China (amazonaws.com.cn), the
// dual-stack names (api.aws, api.amazonwebservices.com.cn), and the isolated partitions.
var awsProviderDomains = []string{
	"amazonaws.com", "amazonaws.com.cn", "api.aws", "amazonwebservices.com.cn",
	"c2s.ic.gov", "sc2s.sgov.gov", "csp.hci.ic.gov", "cloud.adc-e.uk",
}

// AWSProviderOwnedHost reports whether host is under a zone AWS serves its endpoints from, in any
// spelling (case, a trailing dot, a port). It is not AWSSigV4Scope: that one names the hosts the
// proxy can re-sign, a closed and much smaller set. A gateway that carries the run's identity
// token is never such a host, so the token never goes to AWS, whatever endpoint shape AWS adds
// next. A gateway on an AWS-generated name (an API Gateway or load balancer default name, say) is
// refused too; give it a name of your own.
func AWSProviderOwnedHost(host string) bool {
	h := strings.TrimSpace(host)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.TrimRight(strings.ToLower(h), ".")
	return h != "" && slices.ContainsFunc(awsProviderDomains, func(d string) bool { return h == d || strings.HasSuffix(h, "."+d) })
}

// awsGatewaySigningScope is the scope a gateway's signing host signs for: exactly
// bedrock-runtime.<region>.amazonaws.com in lowercase, which AWSSigV4Scope takes as Bedrock.
func awsGatewaySigningScope(h string) (awsSigV4HostScope, bool) {
	labels := strings.Split(h, ".")
	if h != hostKey(h) || len(labels) != 4 || labels[0] != "bedrock-runtime" || labels[2] != "amazonaws" || labels[3] != "com" ||
		!awsRegionLabel.MatchString(labels[1]) {
		return awsSigV4HostScope{}, false
	}
	service, region, ok := AWSSigV4Scope(h)
	if !ok || service != "bedrock" || region != labels[1] {
		return awsSigV4HostScope{}, false
	}
	return awsSigV4HostScope{service: service, region: region, signHost: h}, true
}

// addAWSSigV4Gateways compiles each gateway entry into the door's scopes and the MITM tables, on
// its own port. One that validation refuses is skipped here too.
func (d *awsSigV4Door) addAWSSigV4Gateways(gws []AWSSigV4Gateway, mitmHosts map[string]bool, mitmPorts map[string]int) {
	for _, gw := range gws {
		k := hostKey(gw.Host)
		sc, ok := awsGatewaySigningScope(gw.SigningHost)
		if _, taken := d.scopes[k]; !ok || taken || AWSProviderOwnedHost(gw.Host) {
			continue
		}
		sc.port, sc.prefix = gw.Port, strings.TrimSuffix(gw.PathPrefix, "/")
		d.scopes[k] = sc
		mitmHosts[k], mitmPorts[k] = true, gw.Port
	}
}

// awsGatewayRouteMatches reports whether rt is gw's identity route: a provider's, on its host,
// port and path prefix.
func awsGatewayRouteMatches(rt GatewayIdentityRoute, gw AWSSigV4Gateway) bool {
	return rt.ProviderID != "" && hostKey(rt.Host) == hostKey(gw.Host) && rt.Port == gw.Port &&
		strings.TrimSuffix(rt.PathPrefix, "/") == strings.TrimSuffix(gw.PathPrefix, "/")
}

// awsSigV4GatewayRoute is the identity route a request to a gateway on its own port goes out on, or
// nil when the request is not one to send: a path the route does not cover, as decoded or as sent
// (a dot segment, either spelling, covers nothing). The caller refuses another port first.
func (p *Proxy) awsSigV4GatewayRoute(r *http.Request, host string, port int, scope awsSigV4HostScope) *GatewayIdentityRoute {
	gw := AWSSigV4Gateway{Host: host, Port: port, PathPrefix: scope.prefix}
	raw := adoRawPath(r)
	for i := range p.gatewayRoutes {
		if rt := &p.gatewayRoutes[i]; awsGatewayRouteMatches(*rt, gw) && rt.covers(r.URL.Path) && rt.covers(raw) {
			return rt
		}
	}
	return nil
}

// validateAWSSigV4Gateways refuses a gateway entry the proxy could not honour safely: a host that
// is not one lowercase host name, any AWS-owned host (AWSProviderOwnedHost), a host listed twice
// or also re-signed directly, a bad port or path prefix, a signing host that is not a regional
// Bedrock runtime host, no identity route for it (a request signed as the person never goes to a
// host other than AWS without the run's identity), and a host that also carries an injection rule
// or an operator MITM entry.
func (c *Config) validateAWSSigV4Gateways() error {
	g := c.AWSSigV4
	for i, gw := range g.Gateways {
		where := fmt.Sprintf("config: aws_sigv4.gateways[%d]", i)
		switch {
		case gw.Host == "" || gw.Host != hostKey(gw.Host) || strings.ContainsAny(gw.Host, "/:@[]?#\\% "):
			return fmt.Errorf("%s: host %q is not a host name in lowercase", where, gw.Host)
		case AWSProviderOwnedHost(gw.Host):
			return fmt.Errorf("%s: host %q is an AWS address; the run's identity goes only to a gateway with a name of your own", where, gw.Host)
		case slices.Contains(g.Hosts, gw.Host) || slices.ContainsFunc(g.Gateways[:i], func(o AWSSigV4Gateway) bool { return o.Host == gw.Host }):
			return fmt.Errorf("%s: host %q is listed more than once", where, gw.Host)
		case gw.Port < 1 || gw.Port > 65535:
			return fmt.Errorf("%s: port %d is out of range", where, gw.Port)
		case gw.PathPrefix != "" && (!strings.HasPrefix(gw.PathPrefix, "/") || strings.ContainsAny(gw.PathPrefix, "?#\\") ||
			slices.ContainsFunc(strings.Split(gw.PathPrefix, "/"), isDotSegment)):
			return fmt.Errorf("%s: path_prefix %q is not a path without dot segments", where, gw.PathPrefix)
		}
		if _, ok := awsGatewaySigningScope(gw.SigningHost); !ok {
			return fmt.Errorf("%s: signing_host %q is not a regional Bedrock runtime host (bedrock-runtime.<region>.amazonaws.com)", where, gw.SigningHost)
		}
		if !slices.ContainsFunc(c.GatewayIdentity, func(rt GatewayIdentityRoute) bool { return awsGatewayRouteMatches(rt, gw) }) {
			return fmt.Errorf("%s: no gateway_identity provider route has host %q, port %d and path prefix %q; "+
				"a request signed as the person goes to a gateway only with the run's identity", where, gw.Host, gw.Port, gw.PathPrefix)
		}
		for j, inj := range c.Injection {
			if hostKey(inj.Host) == gw.Host {
				return fmt.Errorf("config: injection[%d]: host %q is an aws_sigv4 gateway; a host carries one credential", j, inj.Host)
			}
		}
		for j, m := range c.MITMHosts {
			if h, _ := splitHostPort(strings.TrimSpace(m), 0); hostKey(h) == gw.Host {
				return fmt.Errorf("config: mitm_hosts[%d]: host %q is an aws_sigv4 gateway and is terminated by it alone", j, m)
			}
		}
	}
	return nil
}
