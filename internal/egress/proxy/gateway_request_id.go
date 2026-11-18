// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
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
// the value is longer than gatewayRequestIDMax, holds anything but printable ASCII (0x20
// to 0x7e), or holds no letter or digit. A header with an empty value counts as absent. For a name sent twice, the first
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
		alnum := false
		for i := range len(v) {
			if v[i] < 0x20 || v[i] > 0x7e {
				return ""
			}
			alnum = alnum || v[i] >= '0' && v[i] <= '9' || v[i] >= 'a' && v[i] <= 'z' || v[i] >= 'A' && v[i] <= 'Z'
		}
		if !alnum { // nothing to tell it from a sandbox value that was stripped to punctuation
			return ""
		}
		return v
	}
	return ""
}

// stripSandboxRequestIDs deletes from the outbound request of a gateway attempt every header the
// sandbox sent under one of gatewayRequestIDHeaders, in any case and with an underscore for a
// dash (Go keeps X_Request_Id as "X_request_id", which a gateway may fold into x-request-id).
// The gateway then mints its own id instead of echoing, sanitising or truncating the sandbox's.
// Direct calls keep them. h may be nil.
func stripSandboxRequestIDs(h http.Header) {
	for k := range h {
		switch strings.ToLower(strings.ReplaceAll(k, "_", "-")) {
		case "request-id", "x-request-id", "x-amzn-requestid":
			delete(h, k)
		}
	}
}

// gatewayRequestIDAffixMin is the shortest normalised value whose containment in an id, or of
// an id in it, marks the id as the sandbox's own.
const gatewayRequestIDAffixMin = 16

// sandboxValueBuf is the normalised length of one sandbox header value that is held in a stack
// buffer; a longer value is normalised whole into an allocation, so no length skips a check.
const sandboxValueBuf = 512

// appendNormalised appends s to dst in its normalised form (lower-case, only [a-z0-9]), up to
// cap(dst) bytes, and reports whether it left some out.
func appendNormalised(dst []byte, s string) ([]byte, bool) {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			continue
		}
		if len(dst) == cap(dst) {
			return dst, true
		}
		dst = append(dst, c)
	}
	return dst, false
}

// sandboxRequestIDEcho reports whether id is, or is built from, a value the sandbox's own
// request carried. The proxy forwards the sandbox's headers upstream as it forwards any other
// (the request-id names are stripped on a gateway route, but any other header may be copied
// into x-request-id, with the case, the punctuation or the length changed). Both sides are
// compared in normalised form (lower-case, only [a-z0-9]), and only header values are compared, not the query, path or body. It is true when the id equals any
// value of ANY header in r, or any comma element of the id does; or the id contains a sandbox
// value, or a sandbox value contains the id, once the shorter is gatewayRequestIDAffixMin
// characters or more. r nil has none, and an id with nothing to compare has no echo.
func sandboxRequestIDEcho(r *http.Request, id string) bool {
	if r == nil {
		return false
	}
	idN, _ := appendNormalised(make([]byte, 0, len(id)), id)
	if len(idN) == 0 {
		return false
	}
	var elements [][]byte
	if strings.Contains(id, ",") {
		for e := range strings.SplitSeq(id, ",") {
			if en, _ := appendNormalised(make([]byte, 0, len(e)), e); len(en) > 0 {
				elements = append(elements, en)
			}
		}
	}
	var buf [sandboxValueBuf]byte
	for _, vs := range r.Header {
		for _, v := range vs {
			vn, partial := appendNormalised(buf[:0], v)
			if partial { // slow path: a long value, normalised whole
				vn, _ = appendNormalised(make([]byte, 0, len(v)), v)
			}
			if len(vn) == 0 {
				continue
			}
			if bytes.Equal(vn, idN) || len(vn) >= gatewayRequestIDAffixMin && bytes.Contains(idN, vn) {
				return true
			}
			for _, en := range elements {
				if bytes.Equal(vn, en) {
					return true
				}
			}
			if len(idN) >= gatewayRequestIDAffixMin && bytes.Contains(vn, idN) {
				return true
			}
		}
	}
	return false
}

// stampGatewayRequestID records resp's request id on the row of a call that went to a
// gateway route, the rows that carry a GatewayCallID. r is the sandbox's own request, whose
// header values are never recorded, even when the answer echoes them (sandboxRequestIDEcho). A row without a call id
// (a direct call) and a nil answer are left as they are.
func stampGatewayRequestID(log *egress.DecisionLog, r *http.Request, resp *http.Response) {
	if log == nil || resp == nil || log.GatewayCallID == "" {
		return
	}
	if id := gatewayRequestID(resp.Header); !sandboxRequestIDEcho(r, id) {
		log.GatewayRequestID = id
	}
}
