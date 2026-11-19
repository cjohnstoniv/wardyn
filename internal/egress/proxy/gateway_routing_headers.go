// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
)

// gatewayRoutingPrefixes are the header-name prefixes a gateway reads to decide where to send a
// request and under which key (Portkey's open-source gateway honours x-portkey-custom-host and
// forwards the request, credential included, to that host). They are the gateway's to be set by
// its operator, never by the sandbox, so every sandbox header under one is dropped on a gateway
// host. The default is on, because a sandbox that can steer the request can send the stored key to
// a host of its choosing, and nothing legitimate reaches a gateway through a sandbox header.
var gatewayRoutingPrefixes = []string{"x-portkey-"}

// stripGatewayRoutingHeaders deletes the sandbox's headers under a gateway routing prefix, except
// the one the credential rule owns (compared case-insensitively): the hosted gateway's key rides
// there, and a placeholder rule's header is filled afterwards by applyCredential. A host that is no
// configured gateway's is left alone.
func (p *Proxy) stripGatewayRoutingHeaders(h http.Header, host, ownedHeader string) {
	if _, isGateway := p.gatewayVendorOf(host); !isGateway {
		return
	}
	for name := range h {
		if ownedHeader != "" && strings.EqualFold(name, ownedHeader) {
			continue
		}
		lower := strings.ToLower(name)
		for _, prefix := range gatewayRoutingPrefixes {
			if strings.HasPrefix(lower, prefix) {
				delete(h, name)
				break
			}
		}
	}
}
