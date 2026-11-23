// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/reqcheck"
)

// adoCouldNotTell is the refusal for a request the classifier cannot read.
const adoCouldNotTell = "Wardyn refused this Azure DevOps request: it could not tell what access the request needs."

// adoChecker is the Azure DevOps check as a reqcheck.Checker. It is bound to
// one door's request, host and grant, and to the run's branch rule; the grant
// holds an organisation and capabilities, never a credential. The organisation
// pin and the path guards are the proxy's own adoOrgMatches and adoGitPath.
type adoChecker struct {
	r       *http.Request
	host    string
	grant   ADOGrant
	refRule func([]string) string
}

var _ reqcheck.Checker[adoscope.Verdict] = adoChecker{}

func (c adoChecker) Guard(req reqcheck.Request) string {
	path := req.Path
	if !adoOrgMatches(c.host, path, c.grant.Organization) {
		return fmt.Sprintf("Wardyn refused this Azure DevOps request: this run is granted the %q organisation only.", c.grant.Organization)
	}
	if strings.ContainsFunc(path, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		return "Wardyn refused this Azure DevOps request: its path carries a backslash or a control character."
	}
	if adoGitPath(path) {
		return "Wardyn refused this Azure DevOps request: git must use Wardyn's git broker, not the API connection."
	}
	return ""
}

func (c adoChecker) Classify(req reqcheck.Request) (adoscope.Verdict, error) {
	v, err := adoscope.Classify(adoscope.Request{
		Method:       req.Method,
		Host:         c.host,
		Path:         req.Path,
		RawQuery:     req.RawQuery,
		Header:       req.Header,
		Org:          c.grant.Organization,
		BodyWithheld: req.BodyWithheld,
		BodyPeek:     req.Body,
	})
	if errors.Is(err, adoscope.ErrNeedsBody) {
		return adoscope.Verdict{}, reqcheck.ErrNeedsBody
	}
	return v, err
}

func (c adoChecker) PeekBody() ([]byte, string) { return adoPeekBody(c.r) }

func (c adoChecker) ClassifyRefusal(err error) string {
	if errors.Is(err, adoscope.ErrNoRef) {
		return "Wardyn refused this Azure DevOps request: it names no ref Wardyn can check."
	}
	return adoCouldNotTell
}

// Extra is the run's branch rule over the refs a ref move names. A checker
// built without a rule refuses a ref move rather than let it by.
func (c adoChecker) Extra(v adoscope.Verdict) string {
	if len(v.Refs) == 0 {
		return ""
	}
	if c.refRule == nil {
		return adoCouldNotTell
	}
	return c.refRule(v.Refs)
}

func (c adoChecker) Permits(v adoscope.Verdict) bool {
	return adoscope.Permits(c.grant.Capabilities, v)
}

// Refusal is liftable only for a grantable capability the run does not hold.
func (c adoChecker) Refusal(v adoscope.Verdict) (string, bool) {
	if !v.Capability.Grantable() {
		return fmt.Sprintf("Wardyn refused this Azure DevOps request (%s). No run is granted this.", adoscope.Label(v.Capability)), false
	}
	return fmt.Sprintf("Wardyn refused this Azure DevOps request: it needs %q (%s), and this run was not granted it.",
		adoscope.Label(v.Capability), v.Capability), true
}
