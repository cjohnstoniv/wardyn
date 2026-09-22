// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure DevOps REST gate: every request the proxy terminates on a host the
// run's Azure DevOps grant covers is classified by the capability catalogue
// (internal/adoscope) and forwarded only when the run holds that capability.
//
// THIS IS THE BOUNDARY, not a second opinion. An Entra token carries every
// scope the person ever consented to, so the token does not bound a run; and it
// carries no organisation claim, so a person in two organisations holds a token
// that works in both. The organisation pin and the capability check below are
// the only things that narrow what the injected credential can do.
//
// Everything that cannot be classified honestly is refused: a classification
// error, a write the catalogue does not recognize, a denied area, a body the
// classification reads but the peek cannot see whole, and git-over-HTTP (git uses the broker path, never the
// intercepted connection).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	ruleSourceADO       = "brokered:ado"
	ruleSourceADODenied = "brokered:ado:denied"
)

// ADOGrant is what the run's Azure DevOps grant holds for one host: the ONE
// organisation the row pins and the capabilities the run was granted.
type ADOGrant struct {
	Organization string
	Capabilities []adoscope.Capability
}

// ADOGrantSource answers, for a host, the run's Azure DevOps grant. ok=false
// means the host is not covered and the gate stands aside.
type ADOGrantSource interface {
	ADOGrantFor(host string) (ADOGrant, bool)
}

// adoRefProtected is the base protected-ref rule: no grant carries a
// protected-branch list yet, so every ref counts as protected. Fail closed.
func adoRefProtected(string) bool { return true }

// adoRunRefProtected is the ONE protected-ref predicate both Azure DevOps doors
// use — the REST gate's classifier and the git broker's push check — so a ref
// needs the same capability whichever door moves it. It is adoRefProtected with
// one exception: a ref inside this run's own branch namespace,
// refs/heads/wardyn/<run-id>/…, which agent-run checks the work tree out onto
// and nothing else writes, needs code_write and not policy_bypass.
func (p *Proxy) adoRunRefProtected(ref string) bool {
	prefix := BranchNSPrefix(p.runID)
	if strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) {
		return false
	}
	return adoRefProtected(ref)
}

// adoGitVerbs are the git smart-HTTP endpoints. They are refused on the
// intercepted connection by name: git goes through the broker.
var adoGitVerbs = []string{"info/refs", "git-upload-pack", "git-receive-pack"}

// gateADO is the hook serveMITMRequest calls. It returns the rule source to
// forward under — brokered:ado for a gated host, src unchanged for any other —
// or "" when it has already refused the request.
func (p *Proxy) gateADO(w http.ResponseWriter, r *http.Request, host string, port int, src string) string {
	if p.adoGrants == nil {
		return src
	}
	grant, ok := p.adoGrants.ADOGrantFor(host)
	if !ok {
		return src
	}
	if msg, held := adoCheck(r, host, grant, p.adoRunRefProtected); msg != "" && !p.refuseADO(w, r, host, port, msg, held) {
		return ""
	}
	return ruleSourceADO
}

// refuseADOPlain refuses, on the plain forward lane, every request to a host
// the run's Azure DevOps grant covers, and reports whether it did. The lane
// never runs gateADO, and an absolute-form `https://` request-line there would
// otherwise be credentialed by applyInjection with nothing checking the
// organisation or the capability. REST reaches these hosts through a CONNECT
// tunnel and git through the broker; this lane is not a third door.
func (p *Proxy) refuseADOPlain(w http.ResponseWriter, r *http.Request) bool {
	if p.adoGrants == nil || r.URL == nil {
		return false
	}
	host, port := splitHostPort(r.URL.Host, defaultPortForScheme(r.URL.Scheme))
	if _, ok := p.adoGrants.ADOGrantFor(host); !ok {
		return false
	}
	p.refuseADO(w, r, host, port, "Wardyn refused this Azure DevOps request: it must be sent through an HTTPS tunnel (CONNECT), not as a plain proxy request.", nil)
	return true
}

// adoCheck returns "" when r may be forwarded, else the refusal sentence. held
// is non-nil only for the ONE refusal a person may lift — a grantable
// capability the run does not hold — and names what the request needs.
func adoCheck(r *http.Request, host string, grant ADOGrant, refProtected func(string) bool) (string, *adoscope.Verdict) {
	path := adoRawPath(r)
	if !adoOrgMatches(host, path, grant.Organization) {
		return fmt.Sprintf("Wardyn refused this Azure DevOps request: this run is granted the %q organisation only.", grant.Organization), nil
	}
	if strings.ContainsFunc(path, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return "Wardyn refused this Azure DevOps request: its path carries a backslash or a control character.", nil
	}
	if adoGitPath(path) {
		return "Wardyn refused this Azure DevOps request: git must use Wardyn's git broker, not the API connection.", nil
	}
	// The path classifies first with the body withheld. Only a route whose
	// capability depends on the body (a pull-request completion, a ref move, a
	// work-item $batch, OPTIONS) answers ErrNeedsBody and is peeked; every
	// other body — a package publish, a wiki attachment — streams through
	// untouched, at whatever size.
	req := adoscope.Request{
		Method:       r.Method,
		Host:         host,
		Path:         path,
		Header:       r.Header,
		Org:          grant.Organization,
		RefProtected: refProtected,
		BodyWithheld: true,
	}
	v, err := adoscope.Classify(req)
	if errors.Is(err, adoscope.ErrNeedsBody) {
		peek, msg := adoPeekBody(r)
		if msg != "" {
			return msg, nil
		}
		req.BodyWithheld, req.BodyPeek = false, peek
		v, err = adoscope.Classify(req)
	}
	if err != nil {
		return "Wardyn refused this Azure DevOps request: it could not tell what access the request needs.", nil
	}
	if adoscope.Permits(grant.Capabilities, v) {
		return "", nil
	}
	if !v.Capability.Grantable() {
		return fmt.Sprintf("Wardyn refused this Azure DevOps request (%s). No run is granted this.", adoscope.Label(v.Capability)), nil
	}
	return fmt.Sprintf("Wardyn refused this Azure DevOps request: it needs %q (%s), and this run was not granted it.",
		adoscope.Label(v.Capability), v.Capability), &v
}

// adoRawPath is the request's path as it arrived on the wire, still
// percent-encoded and without the query. An absolute-form target falls back to
// the parsed URL's escaped path, which is what the forward sends.
func adoRawPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		return path
	}
	return r.URL.EscapedPath()
}

// adoOrgMatches is the organisation pin, applied before anything else: on a
// legacy <org>.visualstudio.com host the organisation is the first label, on
// every other host it is the first path segment. Compared as raw bytes, so an
// encoded spelling of the right name is refused rather than decoded.
func adoOrgMatches(host, path, org string) bool {
	want := strings.ToLower(strings.TrimSpace(org))
	if want == "" {
		return false
	}
	if strings.HasSuffix(host, ".visualstudio.com") {
		label, _, _ := strings.Cut(host, ".")
		return label == want
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return strings.ToLower(first) == want
}

// adoGitPath reports whether path is a git smart-HTTP endpoint.
func adoGitPath(path string) bool {
	lower := strings.ToLower(path)
	if strings.Contains(lower+"/", "/_git/") {
		return true
	}
	for _, v := range adoGitVerbs {
		if strings.HasSuffix(lower, "/"+v) {
			return true
		}
	}
	return false
}

// adoPeekBody reads the whole body when it fits adoscope.MaxBodyPeek, puts it
// back on r for the forward, and returns it for the classifier. A body it
// cannot see whole — declared too long, read too long, or encoded — is refused.
func adoPeekBody(r *http.Request) ([]byte, string) {
	for _, enc := range r.Header.Values("Content-Encoding") {
		if !strings.EqualFold(strings.TrimSpace(enc), "identity") {
			return nil, "Wardyn refused this Azure DevOps request: an encoded body cannot be checked."
		}
	}
	if r.ContentLength > adoscope.MaxBodyPeek {
		return nil, "Wardyn refused this Azure DevOps request: its body is too large to check."
	}
	if r.Body == nil || r.Body == http.NoBody {
		return nil, ""
	}
	peek, err := io.ReadAll(io.LimitReader(r.Body, adoscope.MaxBodyPeek+1))
	_ = r.Body.Close()
	if err != nil || len(peek) > adoscope.MaxBodyPeek {
		return nil, "Wardyn refused this Azure DevOps request: its body is too large to check."
	}
	r.Body = io.NopCloser(bytes.NewReader(peek))
	if len(peek) == 0 {
		return nil, ""
	}
	return peek, ""
}

// adoRefusal is Azure DevOps' own error shape, so a client reads it as a
// service answer and stops rather than retrying.
type adoRefusal struct {
	ID             string  `json:"$id"`
	InnerException *string `json:"innerException"`
	Message        string  `json:"message"`
	TypeName       string  `json:"typeName"`
	TypeKey        string  `json:"typeKey"`
	ErrorCode      int     `json:"errorCode"`
	EventID        int     `json:"eventId"`
}

// refuseADO is the ONE refusal point, and the hold point. held names a
// capability a person may grant (adoCheck); for that refusal alone the request
// is escalated (awaitADOCapability) and, if a person approves it in time,
// refuseADO reports true and the caller forwards. Every other refusal, and an
// escalation that ends without an approval, is answered here.
//
// 403, never 401: git and several tools read a 401 as "try another
// credential", which is not what happened.
func (p *Proxy) refuseADO(w http.ResponseWriter, r *http.Request, host string, port int, msg string, held *adoscope.Verdict) bool {
	if held != nil {
		var ok bool
		if ok, msg = p.awaitADOCapability(r.Context(), host, *held, adoRequestDetail(r), msg); ok {
			return true
		}
	}
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceADODenied))
	}
	writeADORefusal(w, http.StatusForbidden, "CapabilityNotGrantedException", msg)
	return false
}

// writeADORefusal answers in Azure DevOps' own error shape (adoRefusal).
func writeADORefusal(w http.ResponseWriter, status int, typeKey, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(adoRefusal{
		ID:        "1",
		Message:   msg,
		TypeName:  "Wardyn.Egress." + typeKey + ", Wardyn",
		TypeKey:   typeKey,
		ErrorCode: 0,
		EventID:   3000,
	})
}

// isADOLane reports whether host is covered by the run's Azure DevOps grant —
// the hosts whose refusals are spelled in Azure DevOps' terms rather than the
// AWS or plain-text ones.
func (p *Proxy) isADOLane(host string) bool {
	if p.adoGrants == nil {
		return false
	}
	_, ok := p.adoGrants.ADOGrantFor(host)
	return ok
}
