// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// otlpTracesPath is the OTLP/HTTP path spans go to; it sits beside the logs path.
const otlpTracesPath = "/v1/traces"

// tracesTarget is the collector URL spans are POSTed to: the logs target with its
// final /v1/logs swapped for /v1/traces, so the endpoint's query and any path
// prefix carry over.
func tracesTarget(logsTarget string) string {
	u, err := url.Parse(logsTarget)
	if err != nil {
		return logsTarget
	}
	setEscapedPath(u, strings.TrimSuffix(u.EscapedPath(), otlpLogsPath)+otlpTracesPath)
	return u.String()
}

// setEscapedPath sets u's path from its escaped form, so an encoded slash in a
// path prefix stays encoded: Path and RawPath are set together.
func setEscapedPath(u *url.URL, escaped string) {
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return
	}
	u.Path, u.RawPath = path, escaped
}

// traceparent is a parsed W3C traceparent.
type traceparent struct {
	traceID  [16]byte
	parentID [8]byte
}

// parseTraceparent reads a W3C traceparent, "00-<32 hex>-<16 hex>-<2 hex>", and
// accepts nothing looser: lowercase hex only, version 00, a trace id and a parent
// span id that are not all zeros, and a two-digit flags byte.
func parseTraceparent(s string) (traceparent, bool) {
	var tp traceparent
	parts := strings.Split(s, "-")
	if len(parts) != 4 || parts[0] != "00" ||
		len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tp, false
	}
	for _, p := range parts {
		if !lowerHex(p) {
			return tp, false
		}
	}
	// Lengths and the alphabet are checked, so neither decode can fail.
	_, _ = hex.Decode(tp.traceID[:], []byte(parts[1]))
	_, _ = hex.Decode(tp.parentID[:], []byte(parts[2]))
	if tp.traceID == ([16]byte{}) || tp.parentID == ([8]byte{}) {
		return traceparent{}, false
	}
	return tp, true
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// traceIDs are the ids a run-scoped row carries on its log record and its span.
type traceIDs struct {
	trace  string // 32 lowercase hex
	span   string // 16 lowercase hex
	parent string // 16 lowercase hex, or "" when the correlation id is not a traceparent
}

// hasTrace reports whether a row has a trace: it is run-scoped.
func hasTrace(ev types.AuditEvent) bool { return ev.RunID != nil && *ev.RunID != uuid.Nil }

// idsFor derives a row's ids from the row alone, so a retry and both deliveries
// (logs and spans) agree. ok is false for a row with no run, which has no trace.
//
// The trace id is the correlation id's traceparent when it is a valid one, else
// the first 16 bytes of SHA-256(correlation id), else the run id's 16 bytes. The
// span id is the first 8 bytes of SHA-256(event id); a row with no event id hashes
// its run id, action and time instead, so rows do not all share one span id.
func idsFor(ev types.AuditEvent) (traceIDs, bool) {
	if !hasTrace(ev) {
		return traceIDs{}, false
	}
	var ids traceIDs
	corr := correlationID(ev.Data)
	var tid [16]byte
	if tp, ok := parseTraceparent(corr); ok {
		tid = tp.traceID
		ids.parent = hex.EncodeToString(tp.parentID[:])
	} else if corr != "" {
		sum := sha256.Sum256([]byte(corr))
		copy(tid[:], sum[:16])
	}
	if tid == ([16]byte{}) {
		tid = [16]byte(*ev.RunID)
	}
	ids.trace = hex.EncodeToString(tid[:])

	seed := ev.ID[:]
	if ev.ID == uuid.Nil {
		seed = []byte(ev.RunID.String() + "|" + ev.Action + "|" + strconv.FormatInt(ev.Time.UnixNano(), 10))
	}
	sum := sha256.Sum256(seed)
	var sid [8]byte
	copy(sid[:], sum[:8])
	if sid == ([8]byte{}) {
		sid[7] = 1 // an all-zero span id is invalid
	}
	ids.span = hex.EncodeToString(sid[:])
	return ids, true
}

// The OTLP/HTTP JSON subset of ExportTraceServiceRequest the sink writes. The
// ids are lowercase hex strings, as the protocol's JSON mapping requires.

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource struct {
		Attributes []otlpAttr `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpTracesRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// spanKindInternal is OTLP's SPAN_KIND_INTERNAL.
const spanKindInternal = 1

// span turns a run-scoped audit row into a zero-length span named for its action.
func span(ev types.AuditEvent, ids traceIDs) otlpSpan {
	ts := strconv.FormatInt(ev.Time.UnixNano(), 10)
	attrs := []otlpAttr{
		{"wardyn.run_id", otlpValue{ev.RunID.String()}},
		{"wardyn.outcome", otlpValue{ev.Outcome}},
	}
	if id := correlationID(ev.Data); id != "" {
		attrs = append(attrs, otlpAttr{"wardyn.correlation_id", otlpValue{id}})
	}
	return otlpSpan{
		TraceID:           ids.trace,
		SpanID:            ids.span,
		ParentSpanID:      ids.parent,
		Name:              ev.Action,
		Kind:              spanKindInternal,
		StartTimeUnixNano: ts,
		EndTimeUnixNano:   ts,
		Attributes:        attrs,
	}
}

// encodeOTLPSpans serialises the run-scoped rows of batch as one
// ExportTraceServiceRequest, with the resource the logs request carries. n is the
// number of spans; a batch with no run-scoped row yields n == 0 and no body.
func encodeOTLPSpans(batch []types.AuditEvent) (body []byte, n int, err error) {
	spans := make([]otlpSpan, 0, len(batch))
	for i := range batch {
		if ids, ok := idsFor(batch[i]); ok {
			spans = append(spans, span(batch[i], ids))
		}
	}
	if len(spans) == 0 {
		return nil, 0, nil
	}
	var rs otlpResourceSpans
	rs.Resource.Attributes = resourceAttrs()
	var scope otlpScopeSpans
	scope.Scope.Name = "wardynd"
	scope.Spans = spans
	rs.ScopeSpans = []otlpScopeSpans{scope}
	body, err = json.Marshal(otlpTracesRequest{ResourceSpans: []otlpResourceSpans{rs}})
	return body, len(spans), err
}
