// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// gatewayRequestIDMax is the longest request id a row records.
const gatewayRequestIDMax = 128

// gatewayRequestIDHeaders are the response headers that carry the id the gateway or the
// provider behind it assigned to a call, most specific first: Anthropic's request-id,
// the x-request-id OpenAI and most gateways send, and Bedrock's x-amzn-requestid.
var gatewayRequestIDHeaders = []string{"Request-Id", "X-Request-Id", "X-Amzn-Requestid"}

// gatewayRequestID is the id the upstream's answer h carries, or "" when it carries none or
// the value is longer than gatewayRequestIDMax or holds anything but printable ASCII (0x20
// to 0x7e). A header with an empty value counts as absent. For a name sent twice, the first
// value is the one read. The first header present, in gatewayRequestIDHeaders' order,
// decides, so a bad value is dropped and never replaced by a later header's. It reads the
// answer only: a header the sandbox sent is not an input here.
func gatewayRequestID(h http.Header) string {
	for _, name := range gatewayRequestIDHeaders {
		v := h.Get(name)
		if v == "" {
			continue
		}
		if len(v) > gatewayRequestIDMax {
			return ""
		}
		for i := range len(v) {
			if v[i] < 0x20 || v[i] > 0x7e {
				return ""
			}
		}
		return v
	}
	return ""
}

// sandboxRequestIDEcho reports whether id is a value the sandbox's own request carried under
// any of gatewayRequestIDHeaders. The proxy forwards those headers upstream as it forwards
// any other, and a gateway that echoes request headers would hand the sandbox's value back
// on its answer: such a value is the sandbox's, not the gateway's, and is never recorded.
// Exact bytes after the same trim the header parser applies; r nil has none.
func sandboxRequestIDEcho(r *http.Request, id string) bool {
	if r == nil {
		return false
	}
	for _, name := range gatewayRequestIDHeaders {
		for _, v := range r.Header.Values(name) {
			if strings.Trim(v, " \t") == id {
				return true
			}
		}
	}
	return false
}

// stampGatewayRequestID records resp's request id on the row of a call that went to a
// gateway route, the rows that carry a GatewayCallID. r is the sandbox's own request, whose
// request ids are never recorded even when the answer echoes them. A row without a call id
// (a direct call) and a nil answer are left as they are.
func stampGatewayRequestID(log *egress.DecisionLog, r *http.Request, resp *http.Response) {
	if log == nil || resp == nil || log.GatewayCallID == "" {
		return
	}
	if id := gatewayRequestID(resp.Header); !sandboxRequestIDEcho(r, id) {
		log.GatewayRequestID = id
	}
}
