// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// gatewayRequestIDMax is the longest request id a row records.
const gatewayRequestIDMax = 128

// gatewayRequestIDHeaders are the response headers that carry the id the gateway or the
// provider behind it assigned to a call, most specific first: Anthropic's request-id,
// the x-request-id OpenAI and most gateways send, and Bedrock's x-amzn-requestid.
var gatewayRequestIDHeaders = []string{"Request-Id", "X-Request-Id", "X-Amzn-Requestid"}

// gatewayRequestID is the id the upstream's answer h carries, or "" when it carries none or
// the value is longer than gatewayRequestIDMax or holds anything but printable ASCII. The
// first of gatewayRequestIDHeaders that is present decides, so a bad value is dropped and
// never replaced by a later header's. It reads the answer only: a header the sandbox sent
// is not an input here.
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

// stampGatewayRequestID records resp's request id on the row of a call that went to a
// gateway route, the rows that carry a GatewayCallID. A row without one (a direct call)
// and a nil answer are left as they are.
func stampGatewayRequestID(log *egress.DecisionLog, resp *http.Response) {
	if log == nil || resp == nil || log.GatewayCallID == "" {
		return
	}
	log.GatewayRequestID = gatewayRequestID(resp.Header)
}
