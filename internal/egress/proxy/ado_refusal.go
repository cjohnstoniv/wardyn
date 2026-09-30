// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// How Azure DevOps refuses a request the proxy forwarded, and how that
// refusal reaches the run. Three shapes can be told apart:
//
//   - 203 with a text/html sign-in page: no credential reached Azure DevOps
//     at all. On the REST door this is rewritten into a 401 so a client that
//     takes 203 as success doesn't parse HTML as JSON.
//   - 403 or 404 WITH an X-VSS-UserData header: the credential authenticated
//     (the header appears only once an identity is established) and the
//     identity is not permitted.
//   - 401: the credential was refused. A missing scope, an expired token and
//     a revoked token all answer the same empty 401, so the message names
//     all three and never claims a scope problem it cannot prove.
//
// Each class is recorded as an allow — Wardyn forwarded the request; nothing
// in its policy refused it. No message or log line carries credential bytes.

import (
	"context"
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
	// A request retried once with a re-resolved header after Azure DevOps refused a stale one
	// (healADOHeader); an allow, written between the two forwarding rows.
	ruleSourceADOReresolved    = "brokered:ado:reresolved"
	ruleSourceADOGitReresolved = "brokered:ado-git:reresolved"
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

// adoCredentialRefused reports whether Azure DevOps refused the injected credential itself — a 401,
// or a 203 sign-in page — which is what a stale per-host header gets. Both doors ask BEFORE they
// relay, so healADOHeader can run first.
func adoCredentialRefused(resp *http.Response) bool {
	c := classifyADOUpstream(resp)
	return c == adoUpstreamRefused || c == adoUpstreamNotSignedIn
}

// healADOHeader answers Azure DevOps refusing sent, host's injected header. The header is dropped
// either way, so the host's next request re-resolves with stale_jti. A request whose body can be sent
// again re-resolves now: retry reports a header other than sent to retry with, false when the
// control plane had nothing newer (bearer and own_pat resolves return the same credential). err is
// the re-resolve's own, a 423's ended hold included; the caller answers it as a failed resolve.
func (p *Proxy) healADOHeader(ctx context.Context, host string, sent injectedHeader, replayable bool) (fresh injectedHeader, retry bool, err error) {
	if !replayable {
		p.inject.dropStale(host, sent.jti)
		return injectedHeader{}, false, nil
	}
	if fresh, err = p.inject.reresolveStale(ctx, host, sent.jti); err != nil {
		return injectedHeader{}, false, err
	}
	return fresh, fresh.value != "" && fresh.value != sent.value, nil
}

// drainClose reads what is left of a response the caller will not relay, up to 64 KiB, and closes it.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// ruleSource is the class's decision-log rule source on the REST or git door.
func (c adoUpstreamClass) ruleSource(git bool) string {
	switch c {
	case adoUpstreamNotSignedIn:
		if git {
			return ruleSourceADOGitUpstreamNotSignedIn
		}
		return ruleSourceADOUpstreamNotSignedIn
	case adoUpstreamRefused:
		if git {
			return ruleSourceADOGitUpstreamRefused
		}
		return ruleSourceADOUpstreamRefused
	case adoUpstreamNoAccess:
		if git {
			return ruleSourceADOGitUpstreamNoAccess
		}
		return ruleSourceADOUpstreamNoAccess
	}
	return ""
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
// REST door it classifies the answer: a 401/403/404 body passes through
// unchanged with the sentence in X-Wardyn-Egress-Detail; a 203 sign-in page
// is answered as a 401 in Azure DevOps' own error shape.
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
// would make git prompt for a username; a relayed 203 page is not a git
// answer. Port is 443 because the brokered git lane always dials :443.
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
