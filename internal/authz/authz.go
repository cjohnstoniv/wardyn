// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package authz is Wardyn's authorization kernel: the one Decision every door
// refuses with, and the registry of refusal reasons that is the audit contract
// a SIEM rule is written against. Pure Go — no HTTP, no store — so the same
// Decision can later cross a wire (schema authz/v1) unchanged.
package authz

import (
	"maps"
	"slices"

	"github.com/google/uuid"
)

// Schema names the wire shape of Decision. Changes inside v1 are additive and
// readers ignore unknown fields; a removal or rename is a new schema.
const Schema = "authz/v1"

// AuditAction is the audit action every audited refusal is recorded under.
const AuditAction = "authz.denied"

// Effect is what a Decision does to the request. Append-only: it is part of
// the wire contract (testdata/wire.golden).
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
	// EffectHidden is the 404 twin: byte-identical to a missing row, so a
	// refusal is never an existence oracle.
	EffectHidden        Effect = "hidden"
	EffectUnprocessable Effect = "unprocessable"
	// EffectConflict is a refusal answered 409: the request is well-formed and
	// the caller could otherwise make it, but their own session state (a
	// deleted user-view type) conflicts with it.
	EffectConflict Effect = "conflict"
)

// Status is the HTTP status a refusal with this effect answers with.
func (e Effect) Status() int {
	switch e {
	case EffectAllow:
		return 200
	case EffectHidden:
		return 404
	case EffectUnprocessable:
		return 422
	case EffectConflict:
		return 409
	default:
		return 403 // an unknown effect refuses
	}
}

// ObligationKind is what an allow still does to the request. Append-only, like
// Effect.
type ObligationKind string

// ObligationDrop removes one entry from the request and lets the rest proceed.
const ObligationDrop ObligationKind = "drop"

// Obligation is one thing a Decision does to one part of the request.
type Obligation struct {
	Kind   ObligationKind `json:"kind"`
	Detail string         `json:"detail"`
}

// Step is one rule a decision passed through, for an explanation. Strings only,
// so a Decision round-trips through JSON unchanged.
type Step struct {
	Rule    string `json:"rule"`
	Tier    string `json:"tier,omitempty"`
	RowID   string `json:"row_id,omitempty"`
	Outcome string `json:"outcome"`
}

// Decision is one authorization answer. Every field is data (no funcs), so it
// serializes as-is.
type Decision struct {
	Schema string `json:"schema"`
	Effect Effect `json:"effect"`
	Status int    `json:"status"`
	Reason Reason `json:"reason,omitempty"`
	// Target is what was refused: a request field (runs.image), a route, or a
	// resource id.
	Target string     `json:"target,omitempty"`
	RunID  *uuid.UUID `json:"run_id,omitempty"`
	// Sentence is the refusal body. Empty means the reason's own sentence.
	Sentence    string            `json:"sentence,omitempty"`
	Detail      map[string]string `json:"detail,omitempty"`
	Obligations []Obligation      `json:"obligations,omitempty"`
	Trace       []Step            `json:"trace,omitempty"`
}

// Deny is the refusal of target for reason, answered with sentence. The effect
// and status come from the registry, never from the door; an unregistered
// reason still refuses (403) and is caught where it is emitted.
func Deny(reason Reason, target, sentence string) Decision {
	eff := EffectDeny
	if ref, ok := Lookup(reason); ok {
		eff = ref.Effect
	}
	return Decision{Schema: Schema, Effect: eff, Status: eff.Status(), Reason: reason, Target: target, Sentence: sentence}
}

// Drop is the refusal of the listed entries of target for reason: they are
// removed and the request proceeds without them.
func Drop(reason Reason, target string, dropped []string) Decision {
	d := Deny(reason, target, "")
	for _, v := range dropped {
		d.Obligations = append(d.Obligations, Obligation{Kind: ObligationDrop, Detail: v})
	}
	return d
}

// OnRun is d about run id.
func (d Decision) OnRun(id uuid.UUID) Decision {
	d.RunID = &id
	return d
}

// With is d with one more detail. The map is copied, so a Decision built from
// d is never changed by it.
func (d Decision) With(key, value string) Decision {
	m := maps.Clone(d.Detail)
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	d.Detail = m
	return d
}

// Principal is who a decision is about.
type Principal struct {
	Subject string `json:"subject"`
	// MemberView: an admin exercising "view as member"; the tier the kernel
	// saw is already clamped, this only marks the row.
	MemberView bool `json:"member_view,omitempty"`
	// UserType is the type a member view looks through; read only with
	// MemberView.
	UserType string `json:"user_type,omitempty"`
	Origin   Origin `json:"origin"`
}

// Origin is where a request reached the deciding control plane from. Zero in
// 0.8. Only the org sets DeviceID, from the device channel it authenticated —
// never from anything a laptop asserts.
type Origin struct {
	DeviceID  *uuid.UUID `json:"device_id,omitempty"`
	Placement string     `json:"placement,omitempty"`
}

var reservedDatumKeys = []string{"reason", "method", "member_mode", "device_channel", "dropped"}

// Datum is the data of d's audit row, refused to p over method (empty when no
// request carried it). A detail never stands in for a reserved key, so it can
// never forge the reason or a marker.
//
// member_mode is a marker, present only when true, and user_type (the type the
// view looks through) rides beside it. device_channel is a
// sibling of the ingest marker device_origin, never that key: device_origin
// stays the mark of a row a laptop hashed and forwarded.
func Datum(d Decision, p Principal, method string) map[string]any {
	m := make(map[string]any, len(d.Detail)+len(reservedDatumKeys))
	for k, v := range d.Detail {
		if !slices.Contains(reservedDatumKeys, k) {
			m[k] = v
		}
	}
	m["reason"] = string(d.Reason)
	if method != "" {
		m["method"] = method
	}
	if p.MemberView {
		m["member_mode"] = true
		m["user_type"] = p.UserType
	}
	if p.Origin.DeviceID != nil {
		m["device_channel"] = map[string]any{"device_id": p.Origin.DeviceID.String()}
	}
	var dropped []string
	for _, o := range d.Obligations {
		if o.Kind == ObligationDrop {
			dropped = append(dropped, o.Detail)
		}
	}
	if dropped != nil {
		m["dropped"] = dropped
	}
	return m
}
