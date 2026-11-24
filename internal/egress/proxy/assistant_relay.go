// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	// maxAssistantBody caps an assistant event or tool call body.
	maxAssistantBody = 256 << 10
	// assistantControlPath is where the control plane serves the assistant run's routes.
	assistantControlPath = "/api/v1/internal/assistant/"
)

// assistantToolName is the shape of a tool name in POST tools/<name>.
var assistantToolName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// handleBrokerAssistant forwards the assistant run's calls to the control plane with the run token injected:
// GET turns, GET tools, POST events and POST tools/<name>, each mapped to /api/v1/internal/assistant/...
// Every other method is 405. The server already refuses a run that is not a live session's, and a session
// that does not match, so the proxy carries no assistant flag.
//
// TRUST BOUNDARY: the sandbox's query string is never forwarded. GET turns' after cursor is the one value
// carried, and only as the number it parses to: the path is built from that number, never from the raw text.
func (p *Proxy) handleBrokerAssistant(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, routeAssistant)
	switch {
	case rest == "turns":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		after, ok := assistantAfter(r.URL.RawQuery)
		if !ok {
			http.Error(w, "after must be a number", http.StatusBadRequest)
			return
		}
		p.relayControlPlane(w, r, http.MethodGet, assistantControlPath+"turns?after="+strconv.FormatUint(after, 10),
			nil, "", ruleSourceAssistant, nil)
	case rest == "tools":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.relayControlPlane(w, r, http.MethodGet, assistantControlPath+"tools", nil, "", ruleSourceAssistant, nil)
	case rest == "events":
		p.forwardAssistantPost(w, r, assistantControlPath+"events")
	case strings.HasPrefix(rest, "tools/"):
		name := strings.TrimPrefix(rest, "tools/")
		if !assistantToolName.MatchString(name) {
			http.Error(w, "invalid tool name", http.StatusBadRequest)
			return
		}
		p.forwardAssistantPost(w, r, assistantControlPath+"tools/"+name)
	default:
		http.Error(w, "unknown brokered route", http.StatusNotFound)
	}
}

// forwardAssistantPost forwards a POST with its body capped at maxAssistantBody. An oversized body is refused
// whole, as forwardBrokeredUpload refuses one, never truncated.
func (p *Proxy) forwardAssistantPost(w http.ResponseWriter, r *http.Request, cpPath string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAssistantBody+1))
	if err != nil {
		http.Error(w, "read assistant body", http.StatusBadRequest)
		return
	}
	if len(body) > maxAssistantBody {
		p.emitLocalDecision(r, egress.Deny, ruleSourceAssistant, nil)
		http.Error(w, "body exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	p.relayControlPlane(w, r, http.MethodPost, cpPath, body, r.Header.Get("Content-Type"), ruleSourceAssistant, nil)
}

// assistantAfter reads GET turns' cursor: absent is 0; present, it must be one unsigned decimal number. Any
// other parameter is dropped, and a query that does not parse is refused.
func assistantAfter(rawQuery string) (uint64, bool) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return 0, false
	}
	vals, present := q["after"]
	if !present {
		return 0, true
	}
	if len(vals) != 1 {
		return 0, false
	}
	n, err := strconv.ParseUint(vals[0], 10, 63)
	return n, err == nil
}
