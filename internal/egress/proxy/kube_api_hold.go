// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The approve_each arm of the kubectl door. A row that says approve_each holds
// every write the door admits (after admit and the restart check, so a request
// the door refuses never asks anyone, and before a token is minted) until a
// person says yes to that exact request: the cluster, namespace, method, raw
// path, canonical query, media type and canonical body (types.KubeCallKey).
// The door then sends exactly what was approved, once: the canonical query and
// body the key hashed. The spend table is the MCP broker's (mcpCalls), one per
// door; it lives in this proxy only, so a new proxy asks again.
//
// It fails closed: a body that is encoded, not JSON, repeats a key or is over
// 32 KiB is refused before anyone is asked; a raise the control plane refuses
// or cannot take is refused; and an answer the proxy cannot read is still
// pending, so the request is never sent on it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// kubeCallWindow is how long, in seconds, a held write waits for a person:
	// under the two-minute timeout a harness gives one shell command.
	kubeCallWindow = 90

	ruleSourceKubeAPIApprovalWait    = "brokered:kube-api:approval-wait"
	ruleSourceKubeAPIApprovalDenied  = "brokered:kube-api:approval-denied"
	ruleSourceKubeAPIApprovalClosed  = "brokered:kube-api:approval-closed"
	ruleSourceKubeAPIApprovalRefused = "brokered:kube-api:approval-refused"

	// The `wardyn` codes of a held write that was not sent.
	kubeCallWaiting  = "kube_call_waiting"
	kubeCallDeclined = "kube_call_declined"
	kubeCallClosed   = "kube_call_closed"

	kubeCallHeldPrefix   = "Wardyn did not send this Kubernetes change: "
	kubeCallWaitingText  = kubeCallHeldPrefix + "it is waiting for a person's yes (approval %s). Run the same command again once it is approved; a different change asks again."
	kubeCallDeclinedText = kubeCallHeldPrefix + "a person declined it. The same change is refused for the rest of the run; a different change asks again."
	kubeCallClosedText   = kubeCallHeldPrefix + "its approval closed without a decision. Run the command again to ask again."
	kubeCallNoAskText    = kubeCallHeldPrefix + "Wardyn could not ask a person about it. Run the command again to ask again."
	kubeCallRefusedText  = kubeCallHeldPrefix + "the control plane refused the request for a person's approval (%d)."
)

// kubeCallSecond is what one second of the window lasts. A var only so a test
// of the window need not wait real seconds.
var kubeCallSecond = time.Second

// kubeCall is one held write read for asking and sending.
type kubeCall struct {
	scope types.KubeCallScope
	query string // canonical
	body  []byte // canonical; nil with no body
}

// holdKubeCall holds an admitted write on an approve_each door until a person
// approves that exact request, and reports whether to send it; the request then
// carries the approved query and body, and the approval it spent. Every other
// request passes with no approval. When it reports false it has answered.
func (p *Proxy) holdKubeCall(w http.ResponseWriter, r *http.Request, host string, port int) (*uuid.UUID, bool) {
	if !p.kube.approveEach || r.Method == http.MethodGet {
		return nil, true
	}
	c, why := readKubeCall(r, p.kube.host)
	if why != "" {
		p.answerKubeHold(w, r, host, port, egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused,
			kubeAPIRefusedPrefix+why+".", why)
		return nil, false
	}
	id, ok := p.awaitKubeCall(w, r, host, port, c)
	if !ok {
		return nil, false
	}
	r.URL.RawQuery = c.query
	r.Body, r.ContentLength, r.TransferEncoding = http.NoBody, 0, nil
	if len(c.body) > 0 {
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(c.body)), int64(len(c.body))
	}
	return &id, true
}

// readKubeCall builds the write's scope; why is the refusal for one a person
// cannot be shown exactly.
func readKubeCall(r *http.Request, cluster string) (kubeCall, string) {
	for _, enc := range r.Header.Values("Content-Encoding") {
		if v := strings.ToLower(strings.TrimSpace(enc)); v != "" && v != "identity" {
			return kubeCall{}, "a change that needs a person's yes must not be encoded"
		}
	}
	var mt string
	switch ct := r.Header.Values("Content-Type"); len(ct) {
	case 0:
	case 1:
		var err error
		if mt, _, err = mime.ParseMediaType(ct[0]); err != nil {
			return kubeCall{}, "the Content-Type is not a media type"
		}
	default:
		return kubeCall{}, "the request names more than one Content-Type"
	}
	if mt != "" && !types.KubeJSONMediaType(mt) {
		return kubeCall{}, "a change that needs a person's yes must be sent as JSON, not " + mt
	}
	const tooLarge = "a change that needs a person's yes must be at most 32 KiB, so a person can read all of it"
	if r.ContentLength > types.MaxMCPCallBytes {
		return kubeCall{}, tooLarge
	}
	var raw []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		if raw, err = io.ReadAll(io.LimitReader(r.Body, types.MaxMCPCallBytes+1)); err != nil {
			return kubeCall{}, "the request body could not be read"
		}
	}
	body, err := types.KubeCallBody(mt, raw)
	switch {
	case errors.Is(err, types.ErrKubeCallTooLarge):
		return kubeCall{}, tooLarge
	case errors.Is(err, types.ErrCanonicalRepeatedKey):
		return kubeCall{}, "the request body repeats a key, so a person cannot be shown it exactly"
	case err != nil:
		return kubeCall{}, "a change that needs a person's yes must be sent as JSON"
	}
	query, err := types.KubeCanonicalQuery(r.URL.RawQuery)
	if err != nil {
		return kubeCall{}, "the query is not one Wardyn can read"
	}
	path := adoRawPath(r)
	ns, ok := types.KubeCallNamespace(path)
	if !ok {
		return kubeCall{}, "the path is not a namespaced Kubernetes API path"
	}
	key, err := types.KubeCallKey(cluster, ns, r.Method, path, query, mt, body)
	if err != nil {
		return kubeCall{}, "the method " + r.Method + " is not one kubectl sends"
	}
	return kubeCall{query: query, body: body, scope: types.KubeCallScope{Cluster: cluster, Namespace: ns, Method: r.Method,
		Path: path, Query: query, MediaType: mt, Body: body, CallSHA256: key, HoldSeconds: kubeCallWindow}}, ""
}

// awaitKubeCall spends an approved write, refuses a declined one, or asks (or
// rejoins the request pending for it) and waits on the open request up to the
// window. It returns the approval spent and true when the write is to be sent.
func (p *Proxy) awaitKubeCall(w http.ResponseWriter, r *http.Request, host string, port int, c kubeCall) (uuid.UUID, bool) {
	answer := func(d egress.Decision, source string, id uuid.UUID, code, text, cause string) (uuid.UUID, bool) {
		p.answerKubeHold(w, r, host, port, d, source, id, code, text, cause)
		return uuid.Nil, false
	}
	calls, key := &p.kube.calls, c.scope.CallSHA256
	id, state, why := calls.admit(key)
	switch {
	case state == apApproved: // admit spent it for this request
		return id, true
	case state == apDenied:
		return answer(egress.Deny, ruleSourceKubeAPIApprovalDenied, id, kubeCallDeclined, kubeCallDeclinedText, "a declined change, refused for the run")
	case why != "":
		return answer(egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused, kubeCallHeldPrefix+why, why)
	}
	defer calls.leave(key)
	if p.approval == nil {
		return answer(egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused, kubeCallNoAskText, "this proxy has no way to ask")
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeCallWindow*kubeCallSecond)
	defer cancel()
	ask := func() (uuid.UUID, bool) {
		for range 2 {
			got, err := p.raiseKubeCall(ctx, c.scope, uuid.Nil)
			var refused *raiseRefusedError
			switch {
			case errors.As(err, &refused):
				answer(egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused, fmt.Sprintf(kubeCallRefusedText, refused.status),
					fmt.Sprintf("the request for an approval was refused (%d)", refused.status))
				return uuid.Nil, false
			case err != nil:
				answer(egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused, kubeCallNoAskText, "the request for an approval could not be raised")
				return uuid.Nil, false
			}
			if got = calls.install(key, got); got != uuid.Nil {
				return got, true
			}
		}
		answer(egress.Deny, ruleSourceKubeAPIApprovalRefused, uuid.Nil, kubeAPIRefused, kubeCallNoAskText, "the request for an approval named one already used")
		return uuid.Nil, false
	}
	if id != uuid.Nil {
		// A retry rejoining the pending request: the console reads it as held for this window too.
		_, _ = p.raiseKubeCall(ctx, c.scope, id)
	} else {
		var asked bool
		if id, asked = ask(); !asked {
			return uuid.Nil, false
		}
	}
	return p.pollKubeCall(ctx, r, c, id, ask, answer)
}

// pollKubeCall waits on approval id for key: true once this request claimed it.
func (p *Proxy) pollKubeCall(ctx context.Context, r *http.Request, c kubeCall, id uuid.UUID,
	ask func() (uuid.UUID, bool), answer func(egress.Decision, string, uuid.UUID, string, string, string) (uuid.UUID, bool)) (uuid.UUID, bool) {
	calls, key := &p.kube.calls, c.scope.CallSHA256
	tick := time.NewTicker(holdPollInterval)
	defer tick.Stop()
	for {
		// Polled at once as well as on each tick: a request decided while nobody waited is
		// answered without a delay. An answer that cannot be read is still pending.
		if ar, ok := p.approval.fetch(ctx, id); ok {
			switch ar.State {
			case types.ApprovalApproved:
				calls.update(key, id, apApproved)
				if calls.claim(key, id) {
					return id, true
				}
				// Another request spent it: this one asks again, in its own window.
				var asked bool
				if id, asked = ask(); !asked {
					return uuid.Nil, false
				}
			case types.ApprovalDenied:
				calls.update(key, id, apDenied)
				return answer(egress.Deny, ruleSourceKubeAPIApprovalDenied, id, kubeCallDeclined, kubeCallDeclinedText, "declined by a person")
			case types.ApprovalExpired, types.ApprovalCancelled:
				calls.update(key, id, apNone)
				return answer(egress.Deny, ruleSourceKubeAPIApprovalClosed, id, kubeCallClosed, kubeCallClosedText, "closed without a decision")
			}
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return uuid.Nil, false // the client went away; the request stays pending for a retry
			}
			return answer(egress.Pending, ruleSourceKubeAPIApprovalWait, id, kubeCallWaiting, fmt.Sprintf(kubeCallWaitingText, id), "waiting for a person")
		case <-tick.C:
		}
	}
}

// raiseKubeCall posts the write's tool_call approval, or rejoins the pending one when rejoin is set.
func (p *Proxy) raiseKubeCall(ctx context.Context, scope types.KubeCallScope, rejoin uuid.UUID) (uuid.UUID, error) {
	var rejoinID *uuid.UUID
	if rejoin != uuid.Nil {
		rejoinID = &rejoin
	}
	body, err := json.Marshal(struct {
		Kind           types.ApprovalKind  `json:"kind"`
		RequestedScope types.KubeCallScope `json:"requested_scope"`
		RejoinID       *uuid.UUID          `json:"rejoin_id,omitempty"`
	}{types.ApprovalToolCall, scope, rejoinID})
	if err != nil {
		return uuid.Nil, err
	}
	return p.approval.raiseBytes(ctx, body)
}

// answerKubeHold writes one held-write row and answers with a 403 Kubernetes
// Status, whose message kubectl prints. A client that went away gets nothing.
func (p *Proxy) answerKubeHold(w http.ResponseWriter, r *http.Request, host string, port int, d egress.Decision, source string,
	id uuid.UUID, code, message, cause string) {
	if p.sink != nil {
		row := decisionLog(p.reqOf(r, host, port), d, source)
		if id != uuid.Nil {
			row.ApprovalID = &id
		}
		row.Cause = cause
		p.sink.emit(row)
	}
	writeKubeStatus(w, code, message)
}
