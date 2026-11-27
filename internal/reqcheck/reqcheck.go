// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package reqcheck is the one order in which a request check decides: refuse
// what its guard refuses, classify it, ask the bound grant, and let only an
// explicit Allow through. A forge supplies a Checker bound to one run's grant;
// Decide runs it. The package is standard-library only, so no lookup, registry
// or credential can sit between a request and its answer.
package reqcheck

import (
	"errors"
	"net/http"
)

// Effect is what a check decides to do with a request. The zero value
// refuses, so a decision nobody filled in is never an allow.
type Effect uint8

const (
	// Refuse answers the request with its sentence and forwards nothing.
	Refuse Effect = iota
	// Hold asks a person to lift the refusal; it forwards only if one does.
	Hold
	// Allow forwards the request. Nothing else does.
	Allow
)

// Request is one request as a checker reads it. Path is the raw path and
// Header is the request's own, read only.
type Request struct {
	Method, Host, Path, RawQuery string
	Header                       http.Header
	// Body is set only after ErrNeedsBody and a whole read.
	Body []byte
	// BodyWithheld marks a request whose body has not been read.
	BodyWithheld bool
}

// ErrNeedsBody is a checker's answer for a request it classifies on its body
// when the body is withheld: Decide reads it and asks again.
var ErrNeedsBody = errors.New("reqcheck: this route is classified on its body")

// Checker is one forge's check, built for one request and bound to one run's
// grant. V is the forge's own verdict.
type Checker[V any] interface {
	// Guard refuses before anything is classified (an organisation pin, a path
	// shape); "" passes.
	Guard(req Request) string
	// Classify names what the request needs; ErrNeedsBody asks for the body.
	Classify(req Request) (V, error)
	// PeekBody is the whole body, or the sentence that refuses the request.
	PeekBody() ([]byte, string)
	// ClassifyRefusal is the sentence for any other Classify error. It is
	// never empty.
	ClassifyRefusal(err error) string
	// Extra is the run's own rule over a classified request; "" passes.
	Extra(v V) string
	// Permits reports whether the bound grant, and any pins on it, permit v.
	Permits(v V) bool
	// Refusal is, after Permits said no, the forge's sentence and whether a
	// person may lift it.
	Refusal(v V) (sentence string, liftable bool)
}

// Decision is a check's answer. Verdict is the zero V when the request was
// refused before it was classified, and Classify's verdict from Extra on.
type Decision[V any] struct {
	Effect   Effect
	Verdict  V
	Sentence string
}

// Decide runs c over req in the one order every forge shares:
//
//  1. Guard; a sentence refuses.
//  2. Classify with the body withheld; on ErrNeedsBody, PeekBody once (its
//     sentence refuses) and Classify again.
//  3. Any error refuses with ClassifyRefusal.
//  4. Extra; a sentence refuses, and is never held.
//  5. Permits; true is the only way to Allow. It is asked before the
//     refusal below, so discovery and metadata stay permitted.
//  6. Refusal; a liftable one is a Hold, every other a Refuse.
func Decide[V any](c Checker[V], req Request) Decision[V] {
	var none V
	if s := c.Guard(req); s != "" {
		return Decision[V]{Effect: Refuse, Verdict: none, Sentence: s}
	}
	req.Body, req.BodyWithheld = nil, true
	v, err := c.Classify(req)
	if errors.Is(err, ErrNeedsBody) {
		body, s := c.PeekBody()
		if s != "" {
			return Decision[V]{Effect: Refuse, Verdict: none, Sentence: s}
		}
		req.Body, req.BodyWithheld = body, false
		v, err = c.Classify(req)
	}
	if err != nil {
		return Decision[V]{Effect: Refuse, Verdict: none, Sentence: c.ClassifyRefusal(err)}
	}
	if s := c.Extra(v); s != "" {
		return Decision[V]{Effect: Refuse, Verdict: v, Sentence: s}
	}
	if c.Permits(v) {
		return Decision[V]{Effect: Allow, Verdict: v}
	}
	s, liftable := c.Refusal(v)
	if liftable {
		return Decision[V]{Effect: Hold, Verdict: v, Sentence: s}
	}
	return Decision[V]{Effect: Refuse, Verdict: v, Sentence: s}
}
