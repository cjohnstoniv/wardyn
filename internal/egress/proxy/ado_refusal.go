// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// How Azure DevOps refuses a request the proxy forwarded, and how that refusal
// reaches the run. Measured against the live service (F-LIVE-7), three shapes
// can be told apart, and a fourth cannot be split:
//
//   - 203 with a text/html sign-in page: no credential Azure DevOps could read
//     reached it at all. A client that takes 203 as success parses HTML as
//     JSON, so on the REST door this one answer is rewritten into a 401.
//   - 403 or 404 WITH an X-VSS-UserData header: the credential authenticated
//     (the header appears only once an identity is established) and the
//     identity is not permitted.
//   - 401: the credential was refused. A token missing the scope, an expired
//     token and a revoked token all answer the same empty 401, so the message
//     names all three and never claims a scope problem it cannot prove.
//
// Each class is recorded as an allow — Wardyn forwarded the request; nothing in
// its policy refused it — under a rule source naming the class, written after
// the lane's own forwarding row. No message or log line carries credential
// bytes: every message is built from fixed sentences, the host, the escaped
// request path and the status code.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	ruleSourceADOUpstreamNotSignedIn    = "brokered:ado:upstream-not-signed-in"
	ruleSourceADOUpstreamRefused        = "brokered:ado:upstream-refused"
	ruleSourceADOUpstreamNoAccess       = "brokered:ado:upstream-no-access"
	ruleSourceADOGitUpstreamNotSignedIn = "brokered:ado-git:upstream-not-signed-in"
	ruleSourceADOGitUpstreamRefused     = "brokered:ado-git:upstream-refused"
	ruleSourceADOGitUpstreamNoAccess    = "brokered:ado-git:upstream-no-access"
)

// adoUpstreamClass is what an Azure DevOps answer says about the request.
type adoUpstreamClass int

const (
	adoUpstreamOK adoUpstreamClass = iota
	adoUpstreamNotSignedIn
	adoUpstreamRefused
	adoUpstreamNoAccess
)

// classifyADOUpstream sorts an Azure DevOps response into its refusal class.
func classifyADOUpstream(resp *http.Response) adoUpstreamClass {
	switch resp.StatusCode {
	case http.StatusNonAuthoritativeInfo:
		if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/html" {
			return adoUpstreamNotSignedIn
		}
	case http.StatusUnauthorized:
		return adoUpstreamRefused
	case http.StatusForbidden, http.StatusNotFound:
		if resp.Header.Get("X-VSS-UserData") != "" {
			return adoUpstreamNoAccess
		}
	}
	return adoUpstreamOK
}

// ruleSource is the class's decision-log rule source on the REST or git door.
func (c adoUpstreamClass) ruleSource(git bool) string {
	src := map[adoUpstreamClass][2]string{
		adoUpstreamNotSignedIn: {ruleSourceADOUpstreamNotSignedIn, ruleSourceADOGitUpstreamNotSignedIn},
		adoUpstreamRefused:     {ruleSourceADOUpstreamRefused, ruleSourceADOGitUpstreamRefused},
		adoUpstreamNoAccess:    {ruleSourceADOUpstreamNoAccess, ruleSourceADOGitUpstreamNoAccess},
	}[c]
	if git {
		return src[1]
	}
	return src[0]
}

// message is the run-facing sentence for the class. target is host plus the
// escaped request path.
func (c adoUpstreamClass) message(target string, status int) string {
	switch c {
	case adoUpstreamNotSignedIn:
		return "this run is not signed in to Azure DevOps: Azure DevOps answered with its sign-in page (HTTP 203). The run's owner needs to sign in to Azure DevOps again."
	case adoUpstreamRefused:
		return "Azure DevOps refused this run's Azure DevOps sign-in (HTTP 401). Azure DevOps does not say why: the sign-in may have expired, it may have been revoked, or it may be missing a permission this request needs."
	default:
		return fmt.Sprintf("your Azure DevOps account lacks access to %s (HTTP %d).", target, status)
	}
}

// noteADOUpstream records and logs a refusal class. port is the upstream port.
func (p *Proxy) noteADOUpstream(r *http.Request, host string, port int, c adoUpstreamClass, git bool, status int) {
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Allow, c.ruleSource(git)))
	}
	slog.Warn("proxy: Azure DevOps refused a brokered request", "host", host, "class", c.ruleSource(git), "status", status)
}

// relayUpstream is forwardInspectedLLM's response tail. On the Azure DevOps
// REST door it classifies the answer: the body of a 401, 403 or 404 passes
// through unchanged, with the sentence beside it in X-Wardyn-Egress-Detail; a
// 203 sign-in page is answered as a 401 in Azure DevOps' own error shape, so no
// client reads HTML as a successful answer. Every other lane relays as before.
func (p *Proxy) relayUpstream(w http.ResponseWriter, r *http.Request, host string, port int, resp *http.Response, ruleSource string) {
	c := adoUpstreamOK
	if ruleSource == ruleSourceADO {
		c = classifyADOUpstream(resp)
	}
	if c == adoUpstreamOK {
		relay(w, resp)
		return
	}
	p.noteADOUpstream(r, host, port, c, false, resp.StatusCode)
	msg := "Wardyn: " + c.message(host+adoRawPath(r), resp.StatusCode)
	if c != adoUpstreamNotSignedIn {
		resp.Header.Del(egressHeaderDetail)
		w.Header().Set(egressHeaderDetail, msg)
		relay(w, resp)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(adoRefusal{
		ID:        "1",
		Message:   msg,
		TypeName:  "Wardyn.Egress.NotSignedInException, Wardyn",
		TypeKey:   "NotSignedInException",
		ErrorCode: 0,
		EventID:   3000,
	})
}

// refuseADOGitUpstream answers git, in git's own terms, when Azure DevOps
// refused a brokered git request, and reports whether it did. A relayed 401
// would make git prompt for a username; a relayed 203 page is not a git answer.
func (p *Proxy) refuseADOGitUpstream(w http.ResponseWriter, r *http.Request, host, rest string, push *adoGitPush, resp *http.Response) bool {
	c := classifyADOUpstream(resp)
	if c == adoUpstreamOK {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	p.noteADOUpstream(r, host, 443, c, true, resp.StatusCode)
	writeADOGitRefusal(w, push, "Wardyn's git broker: "+c.message(host+(&url.URL{Path: rest}).EscapedPath(), resp.StatusCode))
	return true
}
