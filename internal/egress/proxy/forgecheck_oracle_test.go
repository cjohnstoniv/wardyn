// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// The oracle: adoCheck and gitHubAPIAdmit as they were before the gates
// decided through reqcheck.Decide, kept verbatim so the gates can be held to
// them on every row and every fuzzed request.

func oracleADOCheck(r *http.Request, host string, grant ADOGrant, refRule func([]string) string) (string, *adoscope.Verdict) {
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
	// Classifies first with the body withheld; only a route whose capability
	// depends on the body answers ErrNeedsBody and is peeked, so every other
	// body streams through untouched at whatever size.
	req := adoscope.Request{
		Method:       r.Method,
		Host:         host,
		Path:         path,
		RawQuery:     r.URL.RawQuery,
		Header:       r.Header,
		Org:          grant.Organization,
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
	if errors.Is(err, adoscope.ErrNoRef) {
		return "Wardyn refused this Azure DevOps request: it names no ref Wardyn can check.", nil
	}
	if err != nil {
		return "Wardyn refused this Azure DevOps request: it could not tell what access the request needs.", nil
	}
	if len(v.Refs) > 0 {
		if msg := refRule(v.Refs); msg != "" {
			return msg, nil
		}
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

func oracleGitHubAPIAdmit(r *http.Request, g *GitHubAPIGrant) (ghscope.Verdict, string) {
	v, err := ghscope.Classify(ghscope.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header})
	switch {
	case err != nil:
		return v, gitHubAPIRefusedPrefix + "it could not be read as one GitHub operation."
	case ghscope.Permits(g.Capabilities, g.Repos, v):
		return v, ""
	case v.Capability == ghscope.CapDeniedGraphQL:
		return v, gitHubAPIRESTOnly
	case !v.Capability.Grantable() && v.Capability != ghscope.CapMetadata:
		return v, "Wardyn refused this GitHub API request. " + ghscope.Label(v.Capability) + "."
	case !ghscope.Permits(ghscope.GrantableCapabilities(), g.Repos, v):
		return v, gitHubAPIRefusedPrefix + "it names a repository or an owner outside this run's own."
	}
	return v, gitHubAPIRefusedPrefix + "this run's GitHub CLI is not granted \"" + ghscope.Label(v.Capability) + "\"."
}

// forgeOracleDiff runs one row's request through the gate and through the
// oracle, each on its own freshly built request (the Azure DevOps gate
// consumes the body it peeks), and describes how the answers differ, or "".
func forgeOracleDiff(spec forgeRowSpec) string {
	if spec.forge == forgeADO {
		host := spec.sample.Request.Host
		msg, held := adoCheck(spec.sample.Request.build(), host, forgeADOGrant(spec), forgeADORule(spec.rule))
		omsg, oheld := oracleADOCheck(spec.sample.Request.build(), host, forgeADOGrant(spec), forgeADORule(spec.rule))
		if msg != omsg || !reflect.DeepEqual(held, oheld) {
			return fmt.Sprintf("gate (%q, %+v), oracle (%q, %+v)", msg, held, omsg, oheld)
		}
		return ""
	}
	v, why := gitHubAPIAdmit(spec.sample.Request.build(), forgeGHGrant(spec))
	ov, owhy := oracleGitHubAPIAdmit(spec.sample.Request.build(), forgeGHGrant(spec))
	if why != owhy || !reflect.DeepEqual(v, ov) {
		return fmt.Sprintf("gate (%+v, %q), oracle (%+v, %q)", v, why, ov, owhy)
	}
	return ""
}

// forgeOracleRows are every corpus row, with the spec each runs.
func forgeOracleRows(tb testing.TB) []corpusRow {
	tb.Helper()
	g, err := generateForgeRows(realForgeTables(), realForgeEngine())
	if err != nil {
		tb.Fatal(err)
	}
	if len(g.rows) == 0 {
		tb.Fatal("the generator made no rows")
	}
	return g.rows
}

// TestForgeCheckMatchesOracle: on every corpus row the two gates give the
// oracle's answer, sentence and verdict alike.
func TestForgeCheckMatchesOracle(t *testing.T) {
	rows := forgeOracleRows(t)
	var bad []string
	forges := map[string]int{}
	for _, r := range rows {
		forges[r.spec.forge]++
		if d := forgeOracleDiff(r.spec); d != "" {
			bad = append(bad, r.ID+": "+d)
		}
	}
	if forges[forgeADO] == 0 || forges[forgeGH] == 0 {
		t.Fatalf("rows per forge %v, want both", forges)
	}
	if len(bad) > 0 {
		if len(bad) > 10 {
			bad = append(bad[:10], fmt.Sprintf("... and %d more", len(bad)-10))
		}
		t.Fatalf("%d of %d rows differ from the oracle:\n%s", len(bad), len(rows), strings.Join(bad, "\n"))
	}
	t.Logf("%d rows match the oracle", len(rows))
}

// forgeFuzzHeader is a header as one string, a "name: value" pair to a line.
func forgeFuzzHeader(h [][2]string) string {
	var lines []string
	for _, kv := range h {
		lines = append(lines, kv[0]+": "+kv[1])
	}
	return strings.Join(lines, "\n")
}

func forgeFuzzHeaderPairs(s string) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, ": ")
		out = append(out, [2]string{k, v})
	}
	return out
}

// FuzzForgeCheckMatchesOracle mutates each row's method, path, query, header
// and body, and holds the gates to the oracle on whatever request results,
// under the row's grant and rule or pin class.
func FuzzForgeCheckMatchesOracle(f *testing.F) {
	rows := forgeOracleRows(f)
	for i, r := range rows {
		q := r.spec.sample.Request
		f.Add(uint(i), q.Method, q.Path, q.Query, forgeFuzzHeader(q.Header), q.Body)
	}
	f.Fuzz(func(t *testing.T, row uint, method, path, query, header, body string) {
		spec := rows[row%uint(len(rows))].spec
		q := spec.sample.Request
		q.Method, q.Path, q.Query, q.Header, q.Body = method, path, query, forgeFuzzHeaderPairs(header), body
		spec.sample.Request = q
		if d := forgeOracleDiff(spec); d != "" {
			t.Fatalf("%s %q ? %q header %q body %q: %s", method, path, query, header, body, d)
		}
	})
}
