// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The OTLP/HTTP JSON shapes below are the subset of the stable logs protocol
// (ExportLogsServiceRequest) the sink writes. 64-bit integers are decimal
// strings, as the protocol's JSON mapping requires.

type otlpValue struct {
	StringValue string `json:"stringValue"`
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpLogRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber"`
	SeverityText         string     `json:"severityText"`
	Body                 otlpValue  `json:"body"`
	Attributes           []otlpAttr `json:"attributes"`
	// TraceID and SpanID tie the record to its span (OTLPConfig.Traces); both are
	// omitted otherwise, and for a row with no run.
	TraceID string `json:"traceId,omitempty"`
	SpanID  string `json:"spanId,omitempty"`
}

type otlpScopeLogs struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResourceLogs struct {
	Resource struct {
		Attributes []otlpAttr `json:"attributes"`
	} `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpLogsRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

// maxCorrelationIDLen is the longest data.correlation_id the sink exports; the
// run-create field is bounded at the same length, so a longer one is not ours.
const maxCorrelationIDLen = 128

// severityForOutcome maps an audit outcome to the OTLP severity number and text:
// success is INFO, denied is WARN, failure is ERROR; any other outcome is left
// unspecified rather than guessed.
func severityForOutcome(outcome string) (int, string) {
	switch outcome {
	case "success":
		return 9, "INFO"
	case "denied":
		return 13, "WARN"
	case "failure":
		return 17, "ERROR"
	}
	return 0, ""
}

// correlationID returns data.correlation_id when the row carries one as a
// short string, and "" otherwise.
func correlationID(data json.RawMessage) string {
	if !bytes.Contains(data, []byte(`"correlation_id"`)) {
		return ""
	}
	var d struct {
		CorrelationID string `json:"correlation_id"`
	}
	if json.Unmarshal(data, &d) != nil || len(d.CorrelationID) > maxCorrelationIDLen {
		return ""
	}
	return d.CorrelationID
}

// logRecord turns one audit row into a LogRecord. It exports the row's
// identity fields and outcome and nothing from the per-action data except the
// correlation id: the webhook sink's full payload is the ceiling, not the aim.
// withTraceIDs adds the trace and span ids the row's span carries.
func logRecord(ev types.AuditEvent, withTraceIDs bool) otlpLogRecord {
	sev, sevText := severityForOutcome(ev.Outcome)
	ts := strconv.FormatInt(ev.Time.UnixNano(), 10)
	attrs := make([]otlpAttr, 0, 5)
	if ev.RunID != nil {
		attrs = append(attrs, otlpAttr{"wardyn.run_id", otlpValue{ev.RunID.String()}})
	}
	attrs = append(attrs, otlpAttr{"wardyn.actor", otlpValue{ev.Actor}})
	if ev.Target != "" {
		attrs = append(attrs, otlpAttr{"wardyn.target", otlpValue{ev.Target}})
	}
	attrs = append(attrs, otlpAttr{"wardyn.outcome", otlpValue{ev.Outcome}})
	if id := correlationID(ev.Data); id != "" {
		attrs = append(attrs, otlpAttr{"wardyn.correlation_id", otlpValue{id}})
	}
	rec := otlpLogRecord{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: ts,
		SeverityNumber:       sev,
		SeverityText:         sevText,
		Body:                 otlpValue{ev.Action},
		Attributes:           attrs,
	}
	if withTraceIDs {
		if ids, ok := idsFor(ev); ok {
			rec.TraceID, rec.SpanID = ids.trace, ids.span
		}
	}
	return rec
}

// resourceAttrs is the resource both requests carry: service.name=wardynd, plus
// service.instance.id when WARDYN_AUDIT_SOURCE is set, as the other sinks stamp it.
func resourceAttrs() []otlpAttr {
	attrs := []otlpAttr{{"service.name", otlpValue{"wardynd"}}}
	if Source != "" {
		attrs = append(attrs, otlpAttr{"service.instance.id", otlpValue{Source}})
	}
	return attrs
}

// encodeOTLPLogs serialises batch as one ExportLogsServiceRequest: a single
// resource (service.name=wardynd, plus service.instance.id when
// WARDYN_AUDIT_SOURCE is set, as the other sinks stamp it) and one scope.
func encodeOTLPLogs(batch []types.AuditEvent, withTraceIDs bool) ([]byte, error) {
	records := make([]otlpLogRecord, len(batch))
	for i := range batch {
		records[i] = logRecord(batch[i], withTraceIDs)
	}
	var rl otlpResourceLogs
	rl.Resource.Attributes = resourceAttrs()
	var scope otlpScopeLogs
	scope.Scope.Name = "wardynd"
	scope.LogRecords = records
	rl.ScopeLogs = []otlpScopeLogs{scope}
	return json.Marshal(otlpLogsRequest{ResourceLogs: []otlpResourceLogs{rl}})
}
