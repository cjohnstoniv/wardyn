// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/reqcheck"
)

// gitHubCouldNotRead is the refusal for a request that is not one GitHub operation.
const gitHubCouldNotRead = gitHubAPIRefusedPrefix + "it could not be read as one GitHub operation."

// gitHubChecker is the GitHub API check as a reqcheck.Checker, bound to the
// run's grant: its capabilities and repositories, never a credential. It is
// never held: a person cannot lift a GitHub refusal.
type gitHubChecker struct {
	g *GitHubAPIGrant
}

var _ reqcheck.Checker[ghscope.Verdict] = gitHubChecker{}

func (gitHubChecker) Guard(reqcheck.Request) string { return "" }

// Classify reads the request as a call to the REST API: the host is the
// door's, not the request's.
func (gitHubChecker) Classify(req reqcheck.Request) (ghscope.Verdict, error) {
	return ghscope.Classify(ghscope.Request{Method: req.Method, Host: ghscope.APIHost, Path: req.Path, RawQuery: req.RawQuery, Header: req.Header})
}

// PeekBody is never reached: the classifier reads no body.
func (gitHubChecker) PeekBody() ([]byte, string) { return nil, gitHubCouldNotRead }

func (gitHubChecker) ClassifyRefusal(error) string { return gitHubCouldNotRead }

func (gitHubChecker) Extra(ghscope.Verdict) string { return "" }

// repos is the run's repositories; a checker with no grant has none.
func (c gitHubChecker) repos() []string {
	if c.g == nil {
		return nil
	}
	return c.g.Repos
}

// Permits is false for a checker bound to no grant.
func (c gitHubChecker) Permits(v ghscope.Verdict) bool {
	return c.g != nil && ghscope.Permits(c.g.Capabilities, c.g.Repos, v)
}

// Refusal is the sentence for a request Permits refused, never liftable.
func (c gitHubChecker) Refusal(v ghscope.Verdict) (string, bool) {
	switch {
	case v.Capability == ghscope.CapDeniedGraphQL:
		return gitHubAPIRESTOnly, false
	case !v.Capability.Grantable() && v.Capability != ghscope.CapMetadata:
		return "Wardyn refused this GitHub API request. " + ghscope.Label(v.Capability) + ".", false
	case !ghscope.Permits(ghscope.GrantableCapabilities(), c.repos(), v):
		return gitHubAPIRefusedPrefix + "it names a repository or an owner outside this run's own.", false
	}
	return gitHubAPIRefusedPrefix + "this run's GitHub CLI is not granted \"" + ghscope.Label(v.Capability) + "\".", false
}
