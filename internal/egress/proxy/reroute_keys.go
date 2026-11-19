// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// rerouteKeys are the top-level body keys a model gateway reads to send a call to a model other
// than the one the body names, or to read messages the scan never reads. Claude Code and the
// Codex CLI send none of them. The list is fixed: it is the set the supported gateways act on,
// and a key outside it is the gateway's own business. Keys match without regard to case, as the
// model key does.
var rerouteKeys = []string{
	"fallbacks", "models", "context_window_fallbacks", "content_policy_fallbacks",
	"router_settings_override", "preset",
}

// ruleSourceRerouteKeys marks the row of a request whose body carries one of rerouteKeys, or
// could not be read for them, on a run with content inspection on. The row names the keys found
// (Cause), never a value.
//
// The default is on wherever the check can run, with no switch of its own. The allowed models
// gate refuses these keys whenever it is set, since they defeat the list. Inspection refuses
// them in block mode, writes a row and forwards in alert mode, and leaves them to the gateway
// when the route's inspection is the gateway's (inspection: gateway), which reads these keys
// itself.
const ruleSourceRerouteKeys = "scan:reroute-keys"

const (
	rerouteReasonKeys       = "rerouting_keys"
	rerouteReasonUnparsable = "unparsable"
)

// rerouteKeysIn lists the keys of rerouteKeys that body names at its top level, in list order. ok
// is false when body is not one JSON object, or names a key twice (objectFields refuses both).
func rerouteKeysIn(body []byte) (found []string, ok bool) {
	fields, err := objectFields(body, 0)
	if err != nil {
		return nil, false
	}
	for _, k := range rerouteKeys {
		if slices.ContainsFunc(fields, func(f jsonField) bool { return strings.EqualFold(f.key, k) }) {
			found = append(found, k)
		}
	}
	return found, true
}

// rerouteGateRefused is the allowed models gate's check, run after the body's model was read and
// listed: with the list set, a body carrying a rerouting key is refused, as one whose model could
// not be read is. The gate read the body (bodyModel), so it is held charged; release returns
// that charge. It returns true when it answered.
func (p *Proxy) rerouteGateRefused(w http.ResponseWriter, r *http.Request, host string, port int, release func()) bool {
	body, _ := r.Context().Value(chargedBodyKey{}).([]byte)
	found, ok := rerouteKeysIn(body)
	if ok && len(found) == 0 {
		return false
	}
	release()
	cause := "the request body could not be read for rerouting keys"
	if ok {
		cause = "the request body carries rerouting keys (" + strings.Join(found, ", ") + ") that could pick a model off this run's allowed models list"
	}
	return p.refuseAllowedModel(w, r, host, port, http.StatusForbidden, reasonEgressDenied, cause)
}

// rerouteRoute reports whether the request is a body-bearing call to a route that carries a model,
// on the route families the host's shape speaks (as the allowed models gate reads them). rest is
// matched as the classifier reads it and as the gate reads it cleaned, so a spelling one of them
// resolves to a model route is not missed by the other.
func (p *Proxy) rerouteRoute(r *http.Request, host, rest string) bool {
	shape := p.shapeOf(r, host)
	if shape == shapeAWS || !bodyBearingMethod(r.Method) {
		return false
	}
	anthropic, openai := shape != shapeOpenAI, shape != shapeAnthropic
	carries := func(path string) bool {
		return (anthropic && routeCarriesModel(path, anthropicModelRoutes)) || (openai && routeCarriesModel(path, openaiModelRoutes))
	}
	if carries(strings.Trim(rest, "/")) {
		return true
	}
	cleaned, ok := cleanModelPath(rest, false)
	return ok && carries(strings.TrimPrefix(cleaned, "/"))
}

// chargedRestKey marks a request whose charged body (chargedBodyKey) is only the first
// maxLLMScanBody+1 bytes of a longer one: the context value is the unread rest, which the scan
// forwards after the prefix, as it does when it read the prefix itself.
type chargedRestKey struct{}

// rerouteKeysRefused is inspection's check for rerouting keys, run before the request is
// classified so that a route the scan cannot read (the Responses routes) is checked too. It does
// nothing when the route's gateway reads the prompt itself (deferred), or when the request is not
// a call to a model route. It reads the body through the scan's charge and, on a request it lets
// through, leaves that body held charged for the scan and the usage meter, which take the
// bytes from the request instead of reading again; the returned release gives the charge back
// and must be called when the request is done. A body it cannot read for the keys (oversize,
// encoded, not one object, a key named twice) is refused in block mode and written as an
// unparsable row in alert mode. It returns true when it answered.
func (p *Proxy) rerouteKeysRefused(w http.ResponseWriter, r *http.Request, host string, port int, rest string, channel contentscan.Channel, deferred *GatewayIdentityRoute) (func(), bool) {
	mode := p.scanner.Mode()
	if deferred != nil || (mode != contentscan.ModeAlert && mode != contentscan.ModeBlock) || !p.rerouteRoute(r, host, rest) {
		return noopRelease, false
	}
	var (
		found    []string
		ok       = true
		done     = noopRelease
		oversize bool
	)
	if ce := r.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		ok = false
	} else if body, held := r.Context().Value(chargedBodyKey{}).([]byte); held {
		// A body the gate already holds charged is judged as it is: a second charge would wait on
		// this request's own bytes.
		found, ok = rerouteKeysIn(body)
	} else {
		var body []byte
		var err error
		src := r.Body
		if src == nil {
			src = http.NoBody
		}
		body, done, err = p.scan.readBody(r.Context(), src, r.ContentLength, int64(maxLLMScanBody))
		if err != nil {
			p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLM, nil)
			status := http.StatusBadRequest
			if errors.Is(err, errScanBusy) {
				status = http.StatusBadGateway
			}
			p.scanFault(w, p.shapeOf(r, host), "read llm body", err, status)
			return noopRelease, true
		}
		if len(body) > maxLLMScanBody {
			// Oversize: the prefix stays charged and the rest is read on, so the scan and the
			// forward see the whole body, and nothing is read twice.
			unread := src
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), unread), unread}
			*r = *r.WithContext(context.WithValue(context.WithValue(r.Context(), chargedBodyKey{}, body), chargedRestKey{}, unread))
			ok, oversize = false, true
		} else {
			holdChargedBody(r, body)
			found, ok = rerouteKeysIn(body)
		}
	}
	if ok && len(found) == 0 {
		return done, false
	}
	reason, cause := rerouteReasonUnparsable, "unparsable: the request body could not be read for rerouting keys"
	if ok {
		reason, cause = rerouteReasonKeys, "rerouting keys in the request body: "+strings.Join(found, ", ")
	}
	decision := egress.Allow
	if mode == contentscan.ModeBlock {
		decision = egress.Deny
	}
	if oversize && p.scanner.BlocksOnError() {
		// A fail-closed run already refuses an oversize body, with this row and this answer
		// (scanBufferedBodyWith); keep them.
		done()
		p.emitLLMDecision(r, host, port, egress.Deny, ruleSourceLLMBlocked, p.skipSummary("block", "body_oversize", channel))
		writeScanBlockedShaped(w, p.shapeOf(r, host), 0, nil, "body_oversize")
		return noopRelease, true
	}
	if p.sink != nil {
		log := decisionLog(p.reqOf(r, host, port), decision, ruleSourceRerouteKeys)
		log.Cause = cause
		p.sink.emit(log)
	}
	if decision == egress.Deny {
		done()
		writeScanBlockedShaped(w, p.shapeOf(r, host), 0, nil, reason)
		return noopRelease, true
	}
	return done, false
}

// joinRelease is one release that calls each, last first. A nil release is skipped.
func joinRelease(rels ...func()) func() {
	return func() {
		for i := len(rels) - 1; i >= 0; i-- {
			if rels[i] != nil {
				rels[i]()
			}
		}
	}
}
