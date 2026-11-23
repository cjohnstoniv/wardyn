// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"cmp"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/reqcheck"
)

// The mapping from a Decision to the answer each gate gives, as the doors will
// read it: only Allow forwards, and only a Hold on a grantable capability asks
// a person. Test-local until the gates call through reqcheck.

func mappedADORequest(r *http.Request, host string) reqcheck.Request {
	return reqcheck.Request{Method: r.Method, Host: host, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header}
}

func mappedGitHubRequest(r *http.Request) reqcheck.Request {
	return reqcheck.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header}
}

func mappedADOAnswer(d reqcheck.Decision[adoscope.Verdict]) (string, *adoscope.Verdict) {
	switch {
	case d.Effect == reqcheck.Allow:
		return "", nil
	case d.Effect == reqcheck.Hold && d.Verdict.Capability.Grantable():
		v := d.Verdict
		return cmp.Or(d.Sentence, adoCouldNotTell), &v
	}
	return cmp.Or(d.Sentence, adoCouldNotTell), nil
}

func mappedGitHubAnswer(d reqcheck.Decision[ghscope.Verdict]) (ghscope.Verdict, string) {
	if d.Effect == reqcheck.Allow {
		return d.Verdict, ""
	}
	return d.Verdict, cmp.Or(d.Sentence, gitHubCouldNotRead)
}

func decideADOSpec(spec forgeRowSpec) reqcheck.Decision[adoscope.Verdict] {
	r := spec.sample.Request.build()
	host := spec.sample.Request.Host
	return reqcheck.Decide[adoscope.Verdict](adoChecker{r: r, host: host, grant: forgeADOGrant(spec), refRule: forgeADORule(spec.rule)}, mappedADORequest(r, host))
}

func decideGitHubSpec(spec forgeRowSpec) reqcheck.Decision[ghscope.Verdict] {
	r := spec.sample.Request.build()
	return reqcheck.Decide[ghscope.Verdict](gitHubChecker{g: forgeGHGrant(spec)}, mappedGitHubRequest(r))
}

// decideForgeEngine answers each row through reqcheck.Decide and the mapping.
func decideForgeEngine() forgeEngine {
	eng := realForgeEngine()
	eng.run = func(spec forgeRowSpec) map[string]any {
		if spec.forge == forgeADO {
			return forgeADOAnswer(mappedADOAnswer(decideADOSpec(spec)))
		}
		return forgeGHAnswer(mappedGitHubAnswer(decideGitHubSpec(spec)))
	}
	return eng
}

// TestDecideMatchesCorpus: through reqcheck.Decide, each checker and the
// mapping give every recorded row its recorded answer and capability.
func TestDecideMatchesCorpus(t *testing.T) {
	rec := loadForgeRecording(t)
	g, err := generateForgeRows(realForgeTables(), decideForgeEngine())
	if err != nil {
		t.Fatal(err)
	}
	if len(g.rows) != rec.Rows || len(g.driftedIDs) != 0 {
		t.Fatalf("%d rows regenerated (drifted %v), recording has %d", len(g.rows), g.driftedIDs, rec.Rows)
	}
	d, hashErrs := diffForge(rec, g.rows)
	if len(hashErrs) != 0 || len(d.Changes) != 0 || len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Fatalf("Decide differs from the recording: %d hash errors %v, changes %v, added %v, removed %v", len(hashErrs), hashErrs, d.Changes, d.Added, d.Removed)
	}
	// The match is over rows of every kind, not one of them.
	var adoAllowed, adoHeld, adoRefused, ghAllowed, ghRefused int
	for _, r := range rec.Records {
		switch {
		case strings.HasPrefix(r.ID, "ado/") && r.Answer["held"] == true:
			adoHeld++
		case strings.HasPrefix(r.ID, "ado/") && r.Answer["msg"] == "":
			adoAllowed++
		case strings.HasPrefix(r.ID, "ado/"):
			adoRefused++
		case r.Answer["why"] == "":
			ghAllowed++
		default:
			ghRefused++
		}
	}
	if adoAllowed < 20 || adoHeld < 20 || adoRefused < 20 || ghAllowed < 20 || ghRefused < 20 {
		t.Fatalf("the recording holds ado allowed/held/refused %d/%d/%d and github allowed/refused %d/%d, want at least 20 of each", adoAllowed, adoHeld, adoRefused, ghAllowed, ghRefused)
	}
}

// forgeAllRows are the rows through the Decide engine, with their specs.
func forgeAllRows(t *testing.T) []corpusRow {
	t.Helper()
	g, err := generateForgeRows(realForgeTables(), decideForgeEngine())
	if err != nil {
		t.Fatal(err)
	}
	return g.rows
}

// TestADOCheckerConformance holds the Azure DevOps checker to what makes the
// gate safe, over every row.
func TestADOCheckerConformance(t *testing.T) {
	// A grant of code_write never permits the two capabilities that bypass or
	// rewrite branch policy, however many other capabilities ride beside it.
	var allButPolicy []adoscope.Capability
	for _, c := range adoscope.GrantableCapabilities() {
		if c != adoscope.CapPolicyAdmin && c != adoscope.CapPolicyBypass {
			allButPolicy = append(allButPolicy, c)
		}
	}
	for _, grant := range [][]adoscope.Capability{{adoscope.CapCodeWrite}, allButPolicy} {
		c := adoChecker{grant: ADOGrant{Organization: "acme", Capabilities: grant}}
		for _, cap := range []adoscope.Capability{adoscope.CapPolicyAdmin, adoscope.CapPolicyBypass} {
			if c.Permits(adoscope.Verdict{Capability: cap}) {
				t.Errorf("a grant of %d capabilities permits %s", len(grant), cap)
			}
			if _, liftable := c.Refusal(adoscope.Verdict{Capability: cap}); !liftable {
				t.Errorf("%s, grantable and not held, is not liftable", cap)
			}
		}
	}
	// Each capability: permitted by a grant of itself only if grantable, and
	// liftable only if grantable. A non-grantable capability is never held.
	catalogue := adoscope.Catalogue()
	if len(catalogue) < 30 {
		t.Fatalf("catalogue has %d capabilities", len(catalogue))
	}
	for _, cap := range catalogue {
		v := adoscope.Verdict{Capability: cap}
		c := adoChecker{grant: ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{cap}}}
		if got := c.Permits(v); got != cap.Grantable() {
			t.Errorf("a grant of %s permits it = %v, want %v", cap, got, cap.Grantable())
		}
		sentence, liftable := c.Refusal(v)
		if sentence == "" || liftable != cap.Grantable() {
			t.Errorf("Refusal(%s) = %q, liftable %v, want a sentence and liftable only when grantable", cap, sentence, liftable)
		}
	}
	disc := adoscope.Verdict{Capability: adoscope.CapDiscovery}
	if !(adoChecker{grant: ADOGrant{Capabilities: []adoscope.Capability{adoscope.CapCodeRead}}}).Permits(disc) || (adoChecker{}).Permits(disc) {
		t.Error("discovery is not permitted to a run holding a capability and refused to one holding none")
	}
	// ClassifyRefusal answers for every error the classifier returns.
	errsSeen := 0
	for _, s := range slices.Concat(adoSamples, adoNamedSamples) {
		r := s.Request.build()
		req := adoscope.Request{Method: r.Method, Host: s.Request.Host, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header, Org: s.org(), BodyWithheld: true}
		_, err := adoscope.Classify(req)
		if errors.Is(err, adoscope.ErrNeedsBody) {
			peek, _ := adoPeekBody(r)
			req.BodyWithheld, req.BodyPeek = false, peek
			_, err = adoscope.Classify(req)
		}
		if err == nil {
			continue
		}
		errsSeen++
		if got := (adoChecker{}).ClassifyRefusal(err); got == "" {
			t.Errorf("sample %q: ClassifyRefusal(%v) is empty", s.Name, err)
		}
	}
	if errsSeen < 8 {
		t.Fatalf("only %d samples make adoscope.Classify return an error; the check ran over too little", errsSeen)
	}
	for _, err := range []error{adoscope.ErrNoRef, adoscope.ErrNeedsBody, reqcheck.ErrNeedsBody, errors.New("anything"), fmt.Errorf("wrapped: %w", adoscope.ErrNoRef)} {
		if (adoChecker{}).ClassifyRefusal(err) == "" {
			t.Errorf("ClassifyRefusal(%v) is empty", err)
		}
	}
	if (adoChecker{}).ClassifyRefusal(adoscope.ErrNoRef) == (adoChecker{}).ClassifyRefusal(errors.New("x")) {
		t.Error("ErrNoRef and another error get one sentence")
	}
	// A checker with no branch rule refuses a ref move.
	if (adoChecker{}).Extra(adoscope.Verdict{Capability: adoscope.CapCodeWrite, Refs: []string{"refs/heads/x"}}) == "" {
		t.Error("a checker with no branch rule let a ref move by")
	}
	if (adoChecker{}).Extra(adoscope.Verdict{Capability: adoscope.CapCodeRead}) != "" {
		t.Error("a checker with no branch rule refused a request that moves no ref")
	}
	// Over every row: a refusal always carries a sentence, a Hold is a grantable
	// capability, and a non-grantable one is never held.
	var rows, holds, allows int
	for _, row := range forgeAllRows(t) {
		if row.spec.forge != forgeADO {
			continue
		}
		rows++
		d := decideADOSpec(row.spec)
		switch d.Effect {
		case reqcheck.Allow:
			allows++
			if d.Sentence != "" {
				t.Errorf("%s: an Allow carries the sentence %q", row.ID, d.Sentence)
			}
		case reqcheck.Hold:
			holds++
			if !d.Verdict.Capability.Grantable() {
				t.Errorf("%s: a Hold on the non-grantable %q", row.ID, d.Verdict.Capability)
			}
			fallthrough
		default:
			if d.Sentence == "" {
				t.Errorf("%s: effect %d with no sentence", row.ID, d.Effect)
			}
		}
	}
	if rows < 500 || holds == 0 || allows == 0 {
		t.Fatalf("%d rows, %d holds, %d allows: the walk ran over too little", rows, holds, allows)
	}
}

// TestGitHubCheckerConformance holds the GitHub checker to what makes the gate
// safe, over every row.
func TestGitHubCheckerConformance(t *testing.T) {
	catalogue := ghscope.Catalogue()
	if len(catalogue) < 25 {
		t.Fatalf("catalogue has %d capabilities", len(catalogue))
	}
	for _, cap := range catalogue {
		v := ghscope.Verdict{Capability: cap}
		c := gitHubChecker{g: &GitHubAPIGrant{Capabilities: ghscope.GrantableCapabilities()}}
		// No grant of every grantable capability permits a capability that is
		// not grantable, metadata aside.
		if got, want := c.Permits(v), cap.Grantable() || cap == ghscope.CapMetadata; got != want {
			t.Errorf("a grant of every capability permits %s = %v, want %v", cap, got, want)
		}
		sentence, liftable := c.Refusal(v)
		if sentence == "" || liftable {
			t.Errorf("Refusal(%s) = %q, liftable %v, want a sentence and never liftable", cap, sentence, liftable)
		}
	}
	if (gitHubChecker{}).Permits(ghscope.Verdict{Capability: ghscope.CapCodeRead}) || (gitHubChecker{}).Permits(ghscope.Verdict{Capability: ghscope.CapMetadata}) {
		t.Error("a checker bound to no grant permits something")
	}
	if s, liftable := (gitHubChecker{}).Refusal(ghscope.Verdict{Capability: ghscope.CapCodeRead, Repo: "acme/app", Owner: "acme"}); s == "" || liftable {
		t.Errorf("Refusal with no grant = %q, %v", s, liftable)
	}
	if _, s := (gitHubChecker{}).PeekBody(); s == "" {
		t.Error("PeekBody has no sentence")
	}
	errsSeen := 0
	for _, s := range slices.Concat(ghSamples, ghNamedSamples) {
		r := s.Request.build()
		if _, err := ghscope.Classify(ghscope.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header}); err != nil {
			errsSeen++
			if (gitHubChecker{}).ClassifyRefusal(err) == "" {
				t.Errorf("sample %q: ClassifyRefusal(%v) is empty", s.Name, err)
			}
		}
	}
	if errsSeen < 20 {
		t.Fatalf("only %d samples make ghscope.Classify return an error; the check ran over too little", errsSeen)
	}
	var rows, allows int
	for _, row := range forgeAllRows(t) {
		if row.spec.forge != forgeGH {
			continue
		}
		rows++
		d := decideGitHubSpec(row.spec)
		if d.Effect == reqcheck.Hold {
			t.Errorf("%s: a GitHub refusal was held", row.ID)
		}
		if d.Effect == reqcheck.Allow {
			allows++
			if !ghscope.Permits(forgeGHGrant(row.spec).Capabilities, row.spec.repos, d.Verdict) {
				t.Errorf("%s: allowed, but the grant does not permit %+v", row.ID, d.Verdict)
			}
			continue
		}
		if d.Sentence == "" {
			t.Errorf("%s: effect %d with no sentence", row.ID, d.Effect)
		}
	}
	if rows < 1000 || allows == 0 {
		t.Fatalf("%d rows, %d allows: the walk ran over too little", rows, allows)
	}
}

// importsOf is the import paths of one Go source, read without type-checking.
func importsOf(t *testing.T, name string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func isStdlibImport(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}

func isComponentImport(p string) bool {
	const component = "github.com/cjohnstoniv/wardyn/internal/component"
	return p == component || strings.HasPrefix(p, component+"/")
}

// TestForgeChecksUseNoRegistry: no lookup decides a forge check. reqcheck
// imports only the standard library, and this package does not import the
// component registry.
func TestForgeChecksUseNoRegistry(t *testing.T) {
	reqFiles, err := filepath.Glob("../../reqcheck/*.go")
	if err != nil || len(reqFiles) < 2 {
		t.Fatalf("reqcheck files = %v, %v", reqFiles, err)
	}
	for _, f := range reqFiles {
		for _, p := range importsOf(t, f, nil) {
			if !isStdlibImport(p) {
				t.Errorf("%s imports %s; reqcheck is standard library only", f, p)
			}
		}
	}
	proxyFiles, err := filepath.Glob("*.go")
	if err != nil || len(proxyFiles) < 50 {
		t.Fatalf("proxy files = %d, %v", len(proxyFiles), err)
	}
	for _, f := range proxyFiles {
		for _, p := range importsOf(t, f, nil) {
			if isComponentImport(p) {
				t.Errorf("%s imports %s; the forge checks decide with no registry", f, p)
			}
		}
	}
	// The check sees what it is looking for, and passes the standard library.
	planted := "package p\n\nimport (\n\t\"fmt\"\n\t\"github.com/cjohnstoniv/wardyn/internal/component\"\n)\n"
	got := importsOf(t, "planted.go", planted)
	if len(got) != 2 || !slices.ContainsFunc(got, isComponentImport) || !slices.ContainsFunc(got, func(p string) bool { return !isStdlibImport(p) }) {
		t.Fatalf("planted imports %v: the component import was not seen", got)
	}
	if slices.ContainsFunc(importsOf(t, "planted.go", "package p\n\nimport \"fmt\"\n"), func(p string) bool { return isComponentImport(p) || !isStdlibImport(p) }) {
		t.Fatal("a standard library import was read as a registry or a third party")
	}
	if _, err := os.Stat("forgecheck_ado.go"); err != nil {
		t.Fatal(err)
	}
}
