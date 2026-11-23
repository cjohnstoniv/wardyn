// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// The forge check corpus: every Azure DevOps and GitHub request the gates
// judge, run under every grant, rule and pin class, and the answer the
// unchanged functions gave recorded once. A later change to the gates must
// reproduce each recorded answer, or name the row and the commit that moved it.

var (
	forgeRecordFlag = flag.Bool("forgecheck.record", false, "record testdata/forgecheck/v0.10.2.json (refuses when it exists)")
	forgeDeltasFlag = flag.Bool("forgecheck.deltas", false, "write testdata/forgecheck/deltas.json from a replay (refuses when it exists)")
)

const (
	forgeRecordingPath = "testdata/forgecheck/v0.10.2.json"
	forgeDeltasPath    = "testdata/forgecheck/deltas.json"
	// forgeRecordBase is the tree the recording is made at (v0.10.2).
	forgeRecordBase = "b40f924664f34baa456c4883a7dc1a8507815625"
)

// forgeRequest is one request as a sample states it. Header values are
// added as spelled, in order; the body is Body followed by BodyPad spaces, and
// DeclaredLength, when set, is the Content-Length the request carries.
type forgeRequest struct {
	Method         string      `json:"method"`
	Host           string      `json:"host,omitempty"`
	Path           string      `json:"path"`
	Query          string      `json:"query,omitempty"`
	Header         [][2]string `json:"header,omitempty"`
	Body           string      `json:"body,omitempty"`
	BodyPad        int         `json:"body_pad,omitempty"`
	DeclaredLength int64       `json:"declared_length,omitempty"`
}

// build is a fresh *http.Request for f. Each call returns its own body
// reader, because the Azure DevOps gate consumes the body it peeks.
func (f forgeRequest) build() *http.Request {
	uri := f.Path
	if f.Query != "" {
		uri += "?" + f.Query
	}
	h := http.Header{}
	for _, kv := range f.Header {
		h[kv[0]] = append(h[kv[0]], kv[1])
	}
	r := &http.Request{
		Method: f.Method, RequestURI: uri, Host: f.Host, Header: h, Body: http.NoBody,
		URL: &url.URL{Path: f.Path, RawQuery: f.Query},
	}
	if body := f.Body + strings.Repeat(" ", f.BodyPad); body != "" {
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	if f.DeclaredLength != 0 {
		r.ContentLength = f.DeclaredLength
	}
	return r
}

// forgeSample is one request and the capability it is declared to classify
// to. A named row has no declared capability: it is a request that is not a
// capability (an organisation pin, a path shape, a header), and it runs under
// the one grant class (Grant) and rule or pin class (Dim) it names. Org is the
// organisation an Azure DevOps grant pins; empty reads "acme".
type forgeSample struct {
	Name       string       `json:"name"`
	Capability string       `json:"capability"`
	Request    forgeRequest `json:"request"`
	Org        string       `json:"org,omitempty"`
	Grant      string       `json:"grant,omitempty"`
	Dim        string       `json:"dim,omitempty"`
}

func (s forgeSample) org() string {
	if s.Org == "" {
		return "acme"
	}
	return s.Org
}

func (s forgeSample) isNamed() bool { return s.Capability == "" }

// forgeCap is one catalogue entry: a capability name and whether it is grantable.
type forgeCap struct {
	Name      string
	Grantable bool
}

// forgeTables are what the generator reads: each forge's catalogue and samples,
// and the capabilities no request on the checked door can classify to (with the
// reason), which need no sample.
type forgeTables struct {
	ADOCatalogue, GHCatalogue []forgeCap
	ADOSamples, GHSamples     []forgeSample
	ADOExempt, GHExempt       map[string]string
}

// The grant classes, and each forge's rule or pin classes.
var (
	forgeGrantClasses = []string{"none", "exactly", "every-other", "every"}
	forgeADORules     = []string{"silent", "refuses"}
	forgeGHPins       = []string{"inside", "outside", "empty"}
)

// forgeGHRepos are the repositories each pin class expands to.
var forgeGHRepos = map[string][]string{
	"inside":  {"acme/app", "octo/tools"},
	"outside": {"other/repo"},
	"empty":   nil,
}

func realForgeTables() forgeTables {
	return forgeTables{
		ADOCatalogue: adoCatalogue(), GHCatalogue: ghCatalogue(),
		ADOSamples: slices.Concat(adoSamples, adoNamedSamples), GHSamples: slices.Concat(ghSamples, ghNamedSamples),
		ADOExempt: map[string]string{
			"pr": "the pre-split name for the three pull request capabilities; the classifier never returns it",
		},
		GHExempt: map[string]string{
			"pr":              "the pre-split name for pr_write and pr_review; the classifier never returns it",
			"workflows_write": "a push that changes a workflow file; the REST classifier cannot see it, GitHub's token check does",
			"code_write":      "a git push, which classifies only on github.com; the door's host is always api.github.com",
		},
	}
}

func adoCatalogue() []forgeCap {
	var out []forgeCap
	for _, c := range adoscope.Catalogue() {
		out = append(out, forgeCap{string(c), c.Grantable()})
	}
	return out
}

func ghCatalogue() []forgeCap {
	var out []forgeCap
	for _, c := range ghscope.Catalogue() {
		out = append(out, forgeCap{string(c), c.Grantable()})
	}
	return out
}

// forgeRowSpec is what one row runs: the sample under a grant (the expanded
// capabilities) and one rule or pin class.
type forgeRowSpec struct {
	forge  string
	sample forgeSample
	caps   []string
	rule   string   // Azure DevOps: the branch rule class
	repos  []string // GitHub: the run's repositories, expanded from the pin class
}

// corpusRow is one recorded row. Answer is {msg, held, held_capability} for an
// Azure DevOps row and {capability, owner, repo, why} for a GitHub one, with
// msg and why recorded as the digest of the sentence (see forgeRecordedText).
type corpusRow struct {
	ID         string         `json:"id"`
	Inputs     string         `json:"inputs_sha256"`
	Capability string         `json:"capability"`
	Answer     map[string]any `json:"answer"`
	spec       forgeRowSpec
	// text is the answer with its sentences in full, kept in memory to show a
	// failing row; the recording holds each sentence's digest.
	text map[string]any
}

// forgeEngine is the code under replay: what a sample classifies to, and the
// answer one row gets. Tests plant a different one to stand for a later tree.
type forgeEngine struct {
	capability func(forge string, s forgeSample) string
	run        func(spec forgeRowSpec) map[string]any
}

// forgeADORule is the branch rule a row's rule class stands for. It names the
// refs it was handed, so the answer shows which refs reached it.
func forgeADORule(class string) func([]string) string {
	if class == "refuses" {
		return func(refs []string) string { return "REF RULE REFUSED " + strings.Join(refs, ",") }
	}
	return func([]string) string { return "" }
}

func forgeADOGrant(spec forgeRowSpec) ADOGrant {
	g := ADOGrant{Organization: spec.sample.org()}
	for _, c := range spec.caps {
		g.Capabilities = append(g.Capabilities, adoscope.Capability(c))
	}
	return g
}

func forgeGHGrant(spec forgeRowSpec) *GitHubAPIGrant {
	g := &GitHubAPIGrant{Repos: spec.repos}
	for _, c := range spec.caps {
		g.Capabilities = append(g.Capabilities, ghscope.Capability(c))
	}
	return g
}

// forgeADOCapability is what adoscope classifies the sample to, asked the way
// the gate asks: with the body withheld, then peeked when the route needs it.
// It is "" for a refusal.
func forgeADOCapability(s forgeSample) string {
	r := s.Request.build()
	req := adoscope.Request{
		Method: r.Method, Host: s.Request.Host, Path: adoRawPath(r), RawQuery: r.URL.RawQuery,
		Header: r.Header, Org: s.org(), BodyWithheld: true,
	}
	v, err := adoscope.Classify(req)
	if errors.Is(err, adoscope.ErrNeedsBody) {
		peek, msg := adoPeekBody(r)
		if msg != "" {
			return ""
		}
		req.BodyWithheld, req.BodyPeek = false, peek
		v, err = adoscope.Classify(req)
	}
	if err != nil {
		return ""
	}
	return string(v.Capability)
}

// forgeGHCapability is what ghscope classifies the sample to, "" for a refusal.
func forgeGHCapability(s forgeSample) string {
	r := s.Request.build()
	v, err := ghscope.Classify(ghscope.Request{Method: r.Method, Host: ghscope.APIHost, Path: adoRawPath(r), RawQuery: r.URL.RawQuery, Header: r.Header})
	if err != nil {
		return ""
	}
	return string(v.Capability)
}

// forgeRecordedText is how a sentence is recorded: its digest, "" staying "".
// The sentences quote the capability labels, which are wording of their own, so
// the recording keeps a digest of each sentence instead of a second copy of the
// wording; a change to a sentence is still a change to the row.
func forgeRecordedText(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// forgeRecordedAnswer is answer as it is recorded.
func forgeRecordedAnswer(answer map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range answer {
		if s, ok := v.(string); ok && (k == "msg" || k == "why") {
			v = forgeRecordedText(s)
		}
		out[k] = v
	}
	return out
}

// forgeADOAnswer is the recorded shape of adoCheck's answer.
func forgeADOAnswer(msg string, held *adoscope.Verdict) map[string]any {
	a := map[string]any{"msg": msg, "held": held != nil, "held_capability": ""}
	if held != nil {
		a["held_capability"] = string(held.Capability)
	}
	return a
}

// forgeGHAnswer is the recorded shape of gitHubAPIAdmit's answer.
func forgeGHAnswer(v ghscope.Verdict, why string) map[string]any {
	return map[string]any{"capability": string(v.Capability), "owner": v.Owner, "repo": v.Repo, "why": why}
}

// realForgeEngine runs the functions the doors call.
func realForgeEngine() forgeEngine {
	return forgeEngine{
		capability: func(forge string, s forgeSample) string {
			if forge == forgeADO {
				return forgeADOCapability(s)
			}
			return forgeGHCapability(s)
		},
		run: func(spec forgeRowSpec) map[string]any {
			r := spec.sample.Request.build()
			if spec.forge == forgeADO {
				msg, held := adoCheck(r, spec.sample.Request.Host, forgeADOGrant(spec), forgeADORule(spec.rule))
				return forgeADOAnswer(msg, held)
			}
			v, why := gitHubAPIAdmit(r, forgeGHGrant(spec))
			return forgeGHAnswer(v, why)
		},
	}
}

// forgeExpand is the capabilities a grant class stands for, over the
// catalogue as it is now. ok is false for a class with no row: "exactly" when
// the sample's current capability is not grantable.
func forgeExpand(class, current string, cat []forgeCap) (caps []string, ok bool) {
	switch class {
	case "none":
		return nil, true
	case "exactly":
		for _, c := range cat {
			if c.Name == current && c.Grantable {
				return []string{current}, true
			}
		}
		return nil, false
	case "every-other", "every":
		for _, c := range cat {
			if c.Grantable && (class == "every" || c.Name != current) {
				caps = append(caps, c.Name)
			}
		}
		return caps, true
	}
	panic("forgecheck: unknown grant class " + class)
}

// forgeInputsHash is the digest of one row's inputs: the sample as written and
// the class names, never what they expand to, so a new grantable capability
// changes no recorded row's hash.
func forgeInputsHash(forge string, s forgeSample, grant, dim string) string {
	b, err := json.Marshal(struct {
		Forge  string      `json:"forge"`
		Sample forgeSample `json:"sample"`
		Grant  string      `json:"grant_class"`
		Dim    string      `json:"dim_class"`
	}{forge, s, grant, dim})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// forgeGenerated is what the generator returns beyond the rows: the ids of the
// rows of every sample that classifies to something other than it declares.
type forgeGenerated struct {
	rows       []corpusRow
	driftedIDs []string
}

// generateForgeRows builds every row for both forges. It fails when a catalogue
// capability has no sample declaring it (an exempt one aside), when a sample
// names a class that does not exist, when an exempt capability is declared or
// classified to, and when two rows share an id.
func generateForgeRows(tb forgeTables, eng forgeEngine) (forgeGenerated, error) {
	var g forgeGenerated
	for _, f := range []struct {
		name    string
		cat     []forgeCap
		samples []forgeSample
		exempt  map[string]string
		dims    []string
	}{
		{forgeADO, tb.ADOCatalogue, tb.ADOSamples, tb.ADOExempt, forgeADORules},
		{forgeGH, tb.GHCatalogue, tb.GHSamples, tb.GHExempt, forgeGHPins},
	} {
		if err := generateForge(&g, f.name, f.cat, f.samples, f.exempt, f.dims, eng); err != nil {
			return forgeGenerated{}, err
		}
	}
	seen := map[string]bool{}
	for _, r := range g.rows {
		if seen[r.ID] {
			return forgeGenerated{}, fmt.Errorf("two rows share the id %q", r.ID)
		}
		seen[r.ID] = true
	}
	return g, nil
}

func generateForge(g *forgeGenerated, forge string, cat []forgeCap, samples []forgeSample, exempt map[string]string, dims []string, eng forgeEngine) error {
	declared := map[string]bool{}
	for _, s := range samples {
		if !s.isNamed() {
			declared[s.Capability] = true
		}
		if _, ex := exempt[s.Capability]; ex {
			return fmt.Errorf("%s sample %q declares %q, which is exempt as never classified", forge, s.Name, s.Capability)
		}
	}
	for _, c := range cat {
		if _, ex := exempt[c.Name]; !ex && !declared[c.Name] {
			return fmt.Errorf("%s catalogue capability %q has no sample declaring it", forge, c.Name)
		}
	}
	for _, s := range samples {
		cur := eng.capability(forge, s)
		if _, ex := exempt[cur]; ex {
			return fmt.Errorf("%s sample %q classifies to %q, which is exempt as never classified", forge, s.Name, cur)
		}
		classes := forgeGrantClasses
		sdims := dims
		if s.isNamed() {
			if !slices.Contains(classes, s.Grant) || !slices.Contains(dims, s.Dim) {
				return fmt.Errorf("%s named sample %q has class %q/%q", forge, s.Name, s.Grant, s.Dim)
			}
			classes, sdims = []string{s.Grant}, []string{s.Dim}
		} else if s.Grant != "" || s.Dim != "" {
			return fmt.Errorf("%s sample %q declares a capability and names classes", forge, s.Name)
		}
		for _, grant := range classes {
			caps, ok := forgeExpand(grant, cur, cat)
			if !ok {
				continue
			}
			for _, dim := range sdims {
				id := forge + "/" + s.Name + "/" + grant + "/" + dim
				if s.isNamed() {
					id = forge + "/named/" + s.Name
					if !slices.Contains([]string{"silent", "inside"}, dim) {
						id += "/" + dim
					}
					if grant != "every" {
						id += "/" + grant
					}
				}
				spec := forgeRowSpec{forge: forge, sample: s, caps: caps}
				if forge == forgeADO {
					spec.rule = dim
				} else {
					spec.repos = forgeGHRepos[dim]
				}
				text := eng.run(spec)
				g.rows = append(g.rows, corpusRow{
					ID: id, Inputs: forgeInputsHash(forge, s, grant, dim), Capability: cur,
					Answer: forgeRecordedAnswer(text), spec: spec, text: text,
				})
				if !s.isNamed() && cur != s.Capability {
					g.driftedIDs = append(g.driftedIDs, id)
				}
			}
		}
	}
	return nil
}

// forgeRecording is testdata/forgecheck/v0.10.2.json.
type forgeRecording struct {
	Base    string      `json:"base"`
	Rows    int         `json:"rows"`
	Records []corpusRow `json:"records"`
}

// marshalForgeRecording writes the recording with one record to a line, so a
// diff of it reads a row at a time.
func marshalForgeRecording(rec forgeRecording) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n  \"base\": %q,\n  \"rows\": %d,\n  \"records\": [\n", rec.Base, rec.Rows)
	for i, r := range rec.Records {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b.WriteString("    ")
		b.Write(line)
		if i < len(rec.Records)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ]\n}\n")
	return b.Bytes(), nil
}

// A delta is one way a replay differs from the recording. Each carries the
// commit that moved it, filled in by hand; an empty commit fails the replay.
type (
	forgeChange struct {
		Row    string `json:"row"`
		Field  string `json:"field"`
		V0102  any    `json:"v0102"`
		Now    any    `json:"now"`
		Commit string `json:"commit"`
	}
	forgeRowState struct {
		Capability string         `json:"capability"`
		Answer     map[string]any `json:"answer"`
	}
	forgeAdded struct {
		Row    string        `json:"row"`
		Inputs string        `json:"inputs_sha256"`
		Now    forgeRowState `json:"now"`
		Commit string        `json:"commit"`
	}
	forgeRemoved struct {
		Row    string `json:"row"`
		Commit string `json:"commit"`
	}
	forgeDeltas struct {
		Base    string         `json:"base"`
		Changes []forgeChange  `json:"changes"`
		Added   []forgeAdded   `json:"added"`
		Removed []forgeRemoved `json:"removed"`
	}
)

// diffForge is how the rows now differ from the recording: a change for each
// field of a recorded row that moved, an added entry for a row the recording
// lacks, a removed entry for a recorded row that is no longer generated. A row
// whose inputs hash moved is no delta: it is returned in hashErrs.
func diffForge(rec forgeRecording, now []corpusRow) (d forgeDeltas, hashErrs []string) {
	d = forgeDeltas{Base: rec.Base, Changes: []forgeChange{}, Added: []forgeAdded{}, Removed: []forgeRemoved{}}
	recorded := map[string]corpusRow{}
	for _, r := range rec.Records {
		recorded[r.ID] = r
	}
	current := map[string]bool{}
	for _, r := range now {
		current[r.ID] = true
		old, ok := recorded[r.ID]
		if !ok {
			d.Added = append(d.Added, forgeAdded{Row: r.ID, Inputs: r.Inputs, Now: forgeRowState{r.Capability, r.Answer}})
			continue
		}
		if old.Inputs != r.Inputs {
			hashErrs = append(hashErrs, fmt.Sprintf("row %q: inputs_sha256 is %s, recorded %s", r.ID, r.Inputs, old.Inputs))
			continue
		}
		if old.Capability != r.Capability {
			d.Changes = append(d.Changes, forgeChange{Row: r.ID, Field: "capability", V0102: old.Capability, Now: r.Capability})
		}
		keys := map[string]bool{}
		for k := range old.Answer {
			keys[k] = true
		}
		for k := range r.Answer {
			keys[k] = true
		}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			if !reflect.DeepEqual(old.Answer[k], r.Answer[k]) {
				d.Changes = append(d.Changes, forgeChange{Row: r.ID, Field: "answer." + k, V0102: old.Answer[k], Now: r.Answer[k]})
			}
		}
	}
	for _, r := range rec.Records {
		if !current[r.ID] {
			d.Removed = append(d.Removed, forgeRemoved{Row: r.ID})
		}
	}
	sort.SliceStable(d.Changes, func(i, j int) bool {
		if d.Changes[i].Row != d.Changes[j].Row {
			return d.Changes[i].Row < d.Changes[j].Row
		}
		return d.Changes[i].Field < d.Changes[j].Field
	})
	sort.SliceStable(d.Added, func(i, j int) bool { return d.Added[i].Row < d.Added[j].Row })
	sort.SliceStable(d.Removed, func(i, j int) bool { return d.Removed[i].Row < d.Removed[j].Row })
	return d, hashErrs
}

// forgeDeltaKeys is each delta as the text a comparison reads, without its
// commit.
func forgeDeltaKeys(d forgeDeltas) []string {
	var out []string
	enc := func(kind string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		out = append(out, kind+" "+string(b))
	}
	for _, c := range d.Changes {
		c.Commit = ""
		enc("change", c)
	}
	for _, a := range d.Added {
		a.Commit = ""
		enc("added", a)
	}
	for _, r := range d.Removed {
		r.Commit = ""
		enc("removed", r)
	}
	slices.Sort(out)
	return out
}

// checkForgeDeltas holds the deltas a replay found to the deltas file: every
// found delta must be in it, none of its entries may be stale, and each must
// name a commit. Every kind fails closed.
func checkForgeDeltas(found, file forgeDeltas) error {
	var problems []string
	for _, c := range file.Changes {
		if c.Commit == "" {
			problems = append(problems, fmt.Sprintf("change %s %s has no commit", c.Row, c.Field))
		}
	}
	for _, a := range file.Added {
		if a.Commit == "" {
			problems = append(problems, fmt.Sprintf("added %s has no commit", a.Row))
		}
	}
	for _, r := range file.Removed {
		if r.Commit == "" {
			problems = append(problems, fmt.Sprintf("removed %s has no commit", r.Row))
		}
	}
	want, have := forgeDeltaKeys(found), forgeDeltaKeys(file)
	for _, k := range want {
		if !slices.Contains(have, k) {
			problems = append(problems, "not in deltas.json: "+k)
		}
	}
	for _, k := range have {
		if !slices.Contains(want, k) {
			problems = append(problems, "stale in deltas.json: "+k)
		}
	}
	if len(problems) > 0 {
		if len(problems) > 20 {
			problems = append(problems[:20], fmt.Sprintf("... and %d more", len(problems)-20))
		}
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// replayForge is the whole replay check: the recording against the rows now,
// after the deltas file. A zero file means no deltas.
func replayForge(rec forgeRecording, g forgeGenerated, file forgeDeltas) error {
	found, hashErrs := diffForge(rec, g.rows)
	if len(hashErrs) > 0 {
		return fmt.Errorf("recorded rows' inputs changed:\n%s", strings.Join(hashErrs, "\n"))
	}
	recorded := map[string]bool{}
	for _, r := range rec.Records {
		recorded[r.ID] = true
	}
	for _, id := range g.driftedIDs {
		if !recorded[id] {
			return fmt.Errorf("row %q is new and its sample classifies away from the capability it declares", id)
		}
	}
	return checkForgeDeltas(found, file)
}

// writeForgeDeltas runs the replay and writes the deltas it found to path,
// with every commit empty. It refuses when path exists.
func writeForgeDeltas(path string, rec forgeRecording, g forgeGenerated) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists; the deltas are written once", path)
	}
	found, hashErrs := diffForge(rec, g.rows)
	if len(hashErrs) > 0 {
		return fmt.Errorf("recorded rows' inputs changed:\n%s", strings.Join(hashErrs, "\n"))
	}
	b, err := json.MarshalIndent(found, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func loadForgeRecording(t *testing.T) forgeRecording {
	t.Helper()
	raw, err := os.ReadFile(forgeRecordingPath)
	if err != nil {
		t.Fatalf("no recording: %v", err)
	}
	var rec forgeRecording
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Base != forgeRecordBase || rec.Rows != len(rec.Records) || rec.Rows == 0 {
		t.Fatalf("recording base %q rows %d records %d, want base %s and rows == records > 0", rec.Base, rec.Rows, len(rec.Records), forgeRecordBase)
	}
	return rec
}

func loadForgeDeltas(t *testing.T) forgeDeltas {
	t.Helper()
	raw, err := os.ReadFile(forgeDeltasPath)
	if errors.Is(err, os.ErrNotExist) {
		return forgeDeltas{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var d forgeDeltas
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestForgeCheckCorpus records the corpus once (-forgecheck.record), writes
// the deltas file from a replay (-forgecheck.deltas), and otherwise replays: the
// unchanged functions must give every recorded row's recorded answer.
func TestForgeCheckCorpus(t *testing.T) {
	if *forgeRecordFlag && *forgeDeltasFlag {
		t.Fatal("-forgecheck.record and -forgecheck.deltas are two runs")
	}
	if *forgeRecordFlag {
		if _, err := os.Stat(forgeRecordingPath); err == nil {
			t.Fatalf("%s is already recorded; it records once, at the base", forgeRecordingPath)
		}
	}
	g, err := generateForgeRows(realForgeTables(), realForgeEngine())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("forge check corpus: %d rows", len(g.rows))
	if len(g.rows) == 0 {
		t.Fatal("the generator made no rows")
	}
	if *forgeRecordFlag {
		if len(g.driftedIDs) > 0 {
			t.Fatalf("samples classify away from their declared capability: %v", g.driftedIDs)
		}
		rec := forgeRecording{Base: forgeRecordBase, Rows: len(g.rows)}
		for _, r := range g.rows {
			rec.Records = append(rec.Records, r)
		}
		b, err := marshalForgeRecording(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(forgeRecordingPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(forgeRecordingPath, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d rows at %s", rec.Rows, forgeRecordBase)
		return
	}
	rec := loadForgeRecording(t)
	if *forgeDeltasFlag {
		if err := writeForgeDeltas(forgeDeltasPath, rec, g); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := replayForge(rec, g, loadForgeDeltas(t)); err != nil {
		found, _ := diffForge(rec, g.rows)
		byID := map[string]corpusRow{}
		for _, r := range g.rows {
			byID[r.ID] = r
		}
		for i, c := range found.Changes {
			if r, ok := byID[c.Row]; ok && i < 10 {
				t.Logf("row %q now answers %v", c.Row, r.text)
			}
		}
		t.Fatalf("the replay differs from the recording:\n%v", err)
	}
}

// plantedForgeTables is a small world: three Azure DevOps samples and no
// GitHub ones, enough to drive the deltas writer through each history a real
// tree can have.
func plantedForgeTables() forgeTables {
	return forgeTables{
		ADOCatalogue: []forgeCap{{"code_read", true}, {"code_write", true}, {"unclassified_read", false}},
		ADOSamples: []forgeSample{
			adoSample("git GET", "code_read", "GET", forgeADOHost, forgeADORepo+"items", "", ""),
			adoSample("git push POST", "code_write", "POST", forgeADOHost, forgeADORepo+"pushes", "", `{"refUpdates":[{"name":"refs/heads/wardyn/run1/x"}]}`),
			adoSample("unknown area GET", "unclassified_read", "GET", forgeADOHost, "/acme/_apis/frobnicate", "", ""),
		},
	}
}

// overridingForgeEngine is the real engine with some samples classified to
// another capability, standing for a tree where the classifier moved.
func overridingForgeEngine(over map[string]string) forgeEngine {
	real := realForgeEngine()
	real.capability = func(forge string, s forgeSample) string {
		if c, ok := over[s.Name]; ok {
			return c
		}
		return realForgeEngine().capability(forge, s)
	}
	return real
}

func recordPlanted(t *testing.T, tb forgeTables, eng forgeEngine) forgeRecording {
	t.Helper()
	g, err := generateForgeRows(tb, eng)
	if err != nil || len(g.driftedIDs) != 0 || len(g.rows) == 0 {
		t.Fatalf("planted recording: %d rows, drift %v, err %v", len(g.rows), g.driftedIDs, err)
	}
	rec := forgeRecording{Base: "planted", Rows: len(g.rows)}
	rec.Records = append(rec.Records, g.rows...)
	return rec
}

// rowsOf is the ids of the rows whose id starts with prefix.
func rowsOf(rows []corpusRow, prefix string) []string {
	var out []string
	for _, r := range rows {
		if strings.HasPrefix(r.ID, prefix) {
			out = append(out, r.ID)
		}
	}
	return out
}

// TestForgeCheckDeltasWriter: the writer finds each history a real tree has
// had, in a temp dir on planted tables, and a replay holds to what it wrote.
func TestForgeCheckDeltasWriter(t *testing.T) {
	base := plantedForgeTables()
	rec := recordPlanted(t, base, realForgeEngine())
	pushRows := rowsOf(rec.Records, "ado/git push POST/")
	if len(pushRows) != 8 { // none, exactly, every-other, every x two rules
		t.Fatalf("the planted sample has %d rows, want 8: %v", len(pushRows), pushRows)
	}

	t.Run("a new grantable capability adds rows and moves no recorded row's hash", func(t *testing.T) {
		tb := plantedForgeTables()
		tb.ADOCatalogue = append(tb.ADOCatalogue, forgeCap{"zz_new", true})
		tb.ADOSamples = append(tb.ADOSamples, adoSample("new capability sample", "zz_new", "GET", forgeADOHost, forgeADORepo+"items/new", "", ""))
		g, err := generateForgeRows(tb, overridingForgeEngine(map[string]string{"new capability sample": "zz_new"}))
		if err != nil {
			t.Fatal(err)
		}
		d, hashErrs := diffForge(rec, g.rows)
		if len(hashErrs) != 0 || len(d.Changes) != 0 || len(d.Removed) != 0 {
			t.Fatalf("hash errors %v, changes %v, removed %v — a new grantable must only add rows", hashErrs, d.Changes, d.Removed)
		}
		var added []string
		for _, a := range d.Added {
			added = append(added, a.Row)
		}
		if want := rowsOf(g.rows, "ado/new capability sample/"); len(want) != 8 || !slices.Equal(added, slices.Sorted(slices.Values(want))) {
			t.Fatalf("added %v, want exactly the new sample's 8 rows %v", added, want)
		}
		if err := replayForge(rec, g, forgeDeltas{}); err == nil {
			t.Fatal("a replay with no deltas passed over added rows")
		}
	})

	t.Run("a split names a capability change on each of the sample's rows", func(t *testing.T) {
		eng := overridingForgeEngine(map[string]string{"git push POST": "code_read"})
		g, err := generateForgeRows(base, eng)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.driftedIDs) != 8 {
			t.Fatalf("drifted rows %v, want the sample's 8", g.driftedIDs)
		}
		d, hashErrs := diffForge(rec, g.rows)
		if len(hashErrs) != 0 || len(d.Added) != 0 || len(d.Removed) != 0 {
			t.Fatalf("hash errors %v, added %v, removed %v", hashErrs, d.Added, d.Removed)
		}
		var capRows []string
		for _, c := range d.Changes {
			if !strings.HasPrefix(c.Row, "ado/git push POST/") {
				t.Fatalf("a change outside the split sample: %+v", c)
			}
			if c.Field == "capability" {
				if c.V0102 != "code_write" || c.Now != "code_read" {
					t.Fatalf("capability change %+v, want code_write -> code_read", c)
				}
				capRows = append(capRows, c.Row)
			}
		}
		if slices.Sort(capRows); !slices.Equal(capRows, slices.Sorted(slices.Values(pushRows))) {
			t.Fatalf("capability changes on %v, want each of the sample's rows %v", capRows, pushRows)
		}
		if err := replayForge(rec, g, forgeDeltas{}); err == nil {
			t.Fatal("a replay with no deltas passed over a reclassified sample")
		}
	})

	t.Run("a retired capability changes its rows and removes the one that cannot exist", func(t *testing.T) {
		tb := plantedForgeTables()
		tb.ADOCatalogue = []forgeCap{{"code_read", true}, {"unclassified_read", false}} // code_write is gone; its sample still declares it
		eng := overridingForgeEngine(map[string]string{"git push POST": "unclassified_write"})
		g, err := generateForgeRows(tb, eng)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "deltas.json")
		if err := writeForgeDeltas(path, rec, g); err != nil {
			t.Fatal(err)
		}
		if err := writeForgeDeltas(path, rec, g); err == nil {
			t.Fatal("the writer overwrote deltas.json")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file forgeDeltas
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		if len(file.Removed) != 2 || len(file.Added) != 0 {
			t.Fatalf("removed %v added %v, want the two exactly rows removed", file.Removed, file.Added)
		}
		for _, r := range file.Removed {
			if !strings.HasPrefix(r.Row, "ado/git push POST/exactly/") {
				t.Fatalf("removed %q, want an exactly row", r.Row)
			}
		}
		capChanges := 0
		for _, c := range file.Changes {
			if c.Field == "capability" {
				capChanges++
			}
		}
		if capChanges != 6 {
			t.Fatalf("%d capability changes, want one on each of the 6 remaining rows", capChanges)
		}
		// The writer leaves every commit empty, and a replay refuses that.
		if err := replayForge(rec, g, file); err == nil || !strings.Contains(err.Error(), "no commit") {
			t.Fatalf("replay with empty commits = %v, want a refusal naming them", err)
		}
		for i := range file.Changes {
			file.Changes[i].Commit = "abc1234"
		}
		for i := range file.Removed {
			file.Removed[i].Commit = "abc1234"
		}
		if err := replayForge(rec, g, file); err != nil {
			t.Fatalf("replay with every delta attributed = %v", err)
		}
		// A recorded row that is neither regenerated nor removed fails.
		short := file
		short.Removed = file.Removed[:1]
		if err := replayForge(rec, g, short); err == nil {
			t.Fatal("a replay passed with a removed row left out of deltas.json")
		}
		// A stale entry fails too.
		stale := file
		stale.Changes = append(slices.Clone(file.Changes), forgeChange{Row: "ado/git GET/none/silent", Field: "capability", V0102: "a", Now: "b", Commit: "abc1234"})
		if err := replayForge(rec, g, stale); err == nil {
			t.Fatal("a replay passed with a delta that no row has")
		}
	})

	t.Run("a changed sample moves its hash and fails", func(t *testing.T) {
		tb := plantedForgeTables()
		tb.ADOSamples[0].Request.Path += "/other"
		g, err := generateForgeRows(tb, realForgeEngine())
		if err != nil {
			t.Fatal(err)
		}
		if err := replayForge(rec, g, forgeDeltas{}); err == nil || !strings.Contains(err.Error(), "inputs changed") {
			t.Fatalf("replay = %v, want the changed inputs named", err)
		}
	})

	t.Run("the generator refuses a catalogue capability no sample declares", func(t *testing.T) {
		tb := plantedForgeTables()
		tb.ADOCatalogue = append(tb.ADOCatalogue, forgeCap{"zz_orphan", true})
		if _, err := generateForgeRows(tb, realForgeEngine()); err == nil || !strings.Contains(err.Error(), "zz_orphan") {
			t.Fatalf("generateForgeRows = %v, want the orphan named", err)
		}
	})
}
