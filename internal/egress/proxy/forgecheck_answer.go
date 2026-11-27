// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"cmp"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/reqcheck"
)

// decideADO and decideGitHub are the decisions the two forge gates read. They
// are variables only so a test can drive the answers below through the doors;
// nothing but a test assigns them.
var (
	decideADO    = reqcheck.Decide[adoscope.Verdict]
	decideGitHub = reqcheck.Decide[ghscope.Verdict]
)

// adoAnswer is adoCheck's answer for a decision. Only Allow forwards. A Hold
// is liftable only on a grantable capability, so a person is never asked to
// grant one no run can hold; every other decision, a zero one or an unknown
// Effect included, refuses with a sentence.
func adoAnswer(d reqcheck.Decision[adoscope.Verdict]) (string, *adoscope.Verdict) {
	switch {
	case d.Effect == reqcheck.Allow:
		return "", nil
	case d.Effect == reqcheck.Hold && d.Verdict.Capability.Grantable():
		v := d.Verdict
		return cmp.Or(d.Sentence, adoCouldNotTell), &v
	}
	return cmp.Or(d.Sentence, adoCouldNotTell), nil
}

// gitHubAnswer is gitHubAPIAdmit's answer for a decision. Only Allow forwards;
// every other decision refuses with a sentence.
func gitHubAnswer(d reqcheck.Decision[ghscope.Verdict]) (ghscope.Verdict, string) {
	if d.Effect == reqcheck.Allow {
		return d.Verdict, ""
	}
	return d.Verdict, cmp.Or(d.Sentence, gitHubCouldNotRead)
}

// adoRequest is r as the Azure DevOps checker reads it: the path as it
// arrived on the wire. reqcheck.Decide sets the body.
func adoRequest(r *http.Request, host string) reqcheck.Request {
	return reqcheck.Request{Method: r.Method, Host: host, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header}
}

// gitHubRequest is r as the GitHub checker reads it, on the door's host.
func gitHubRequest(r *http.Request) reqcheck.Request {
	return reqcheck.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header}
}
