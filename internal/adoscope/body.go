// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// MaxBodyPeek bounds what the classifier will look at. Far above any of the
// three small request shapes it must classify; a body that does not fit is
// REFUSED, never classified on its visible prefix.
const MaxBodyPeek = 256 << 10

// maxJSONDepth bounds the duplicate-key walk: a 256 KiB body of nothing but
// "[" is legal JSON and would otherwise recurse 256k deep.
const maxJSONDepth = 64

// peekBody returns the body bytes the classifier may trust, or refuses.
//
// Every refusal here is a case where the wire bytes are not the bytes the
// server will parse, or not all of them: a Content-Encoding hides the body in
// a compressed stream, and a declared length longer than the peek (or sitting
// exactly on the bound) means the body continues past what we can see.
func peekBody(req Request) ([]byte, error) {
	if req.BodyWithheld {
		return nil, ErrNeedsBody
	}
	if enc, err := singleHeader(req.Header, "Content-Encoding"); err != nil {
		return nil, err
	} else if enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, fmt.Errorf("adoscope: Content-Encoding %q hides the body from the peek", enc)
	}
	if len(req.BodyPeek) > MaxBodyPeek {
		return nil, fmt.Errorf("adoscope: body peek is %d bytes, the bound is %d", len(req.BodyPeek), MaxBodyPeek)
	}
	declared, known, err := declaredLength(req.Header)
	if err != nil {
		return nil, err
	}
	switch {
	case known && declared > len(req.BodyPeek):
		return nil, fmt.Errorf("adoscope: body is %d bytes, the peek holds %d", declared, len(req.BodyPeek))
	case !known && len(req.BodyPeek) == MaxBodyPeek:
		return nil, fmt.Errorf("adoscope: body fills the %d-byte peek with no declared length — it may continue", MaxBodyPeek)
	}
	return req.BodyPeek, nil
}

// singleHeader is h's one value for name, refusing a repeated header: two
// values mean two answers, and which one the server acts on is not knowable
// from here.
func singleHeader(h http.Header, name string) (string, error) {
	v := headerValues(h, name)
	switch len(v) {
	case 0:
		return "", nil
	case 1:
		return strings.TrimSpace(v[0]), nil
	}
	return "", fmt.Errorf("adoscope: %d %s values — the body's shape is not knowable", len(v), name)
}

// declaredLength is the request's Content-Length, and whether it declared one.
func declaredLength(h http.Header) (int, bool, error) {
	raw, err := singleHeader(h, "Content-Length")
	if err != nil || raw == "" {
		return 0, false, err
	}
	n, convErr := strconv.Atoi(raw)
	if convErr != nil || n < 0 {
		return 0, false, fmt.Errorf("adoscope: Content-Length %q is not a length", raw)
	}
	return n, true, nil
}

// decodeUnique decodes body into v, refusing a body with a DUPLICATE KEY.
//
// Azure DevOps' parser is last-key-wins, and so is Go's, but they need not
// agree which key is last, so `{"bypassPolicy":false,…,"bypassPolicy":true}`
// could show a classifier "false" while the server acts on "true". Refusing
// the body is the only answer immune to key order. Keys compare under
// foldKey, the fold json.Unmarshal binds keys to fields with, so every pair of
// keys that would land in one field is a duplicate here.
func decodeUnique(body []byte, v any) error {
	if err := uniqueKeys(body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("adoscope: body is not the JSON this route takes: %w", err)
	}
	return nil
}

// uniqueKeys walks body's token stream and refuses a repeated key within any
// one object.
func uniqueKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := walkUnique(dec, 0); err != nil {
		return err
	}
	// Trailing content is a second document the server may read differently.
	if dec.More() {
		return fmt.Errorf("adoscope: body carries more than one JSON value")
	}
	return nil
}

// walkUnique consumes exactly one JSON value from dec.
func walkUnique(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("adoscope: body nests deeper than %d levels", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("adoscope: body is not decodable JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return walkObject(dec, depth)
	case '[':
		for dec.More() {
			if err := walkUnique(dec, depth+1); err != nil {
				return err
			}
		}
	}
	// Closing delimiter; not inspected — a malformed stream surfaces as the
	// decode error above on the next read.
	_, err = dec.Token()
	return err
}

// walkObject consumes one object's members, refusing a folded-duplicate key.
func walkObject(dec *json.Decoder, depth int) error {
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return fmt.Errorf("adoscope: body is not decodable JSON: %w", err)
		}
		name, ok := key.(string)
		if !ok {
			return fmt.Errorf("adoscope: body has a non-string object key")
		}
		folded := foldKey(name)
		if seen[folded] {
			return fmt.Errorf("adoscope: body repeats the key %q — which value the server acts on is not knowable", name)
		}
		seen[folded] = true
		if err := walkUnique(dec, depth+1); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// foldKey is encoding/json's key fold (its foldName): each rune becomes the
// least rune of its Unicode simple-fold orbit. strings.ToLower is not the same
// fold: it leaves U+017F (long s) as is, which the decoder matches to "s".
func foldKey(name string) string {
	return strings.Map(func(r rune) rune {
		for {
			f := unicode.SimpleFold(r)
			if f <= r {
				return f
			}
			r = f
		}
	}, name)
}

// packageBatch reports whether r is a feed's package batch route, positionally:
// _apis/packaging/feeds/{feed}/{protocol}/packagesbatch, or the same batch
// under the feed's recyclebin.
func packageBatch(r route) bool {
	if r.area != "packaging" || r.res != "feeds" || !slices.Contains(packageProtocols, r.at(4)) {
		return false
	}
	if r.at(5) == "recyclebin" {
		return r.at(6) == "packagesbatch" && r.at(7) == ""
	}
	return r.at(5) == "packagesbatch" && r.at(6) == ""
}

// packageBatchOps are the batch operations Azure DevOps documents across the
// package protocols, by the capability each needs; not every protocol takes
// every operation.
var packageBatchOps = map[string]Capability{
	"promote":         CapPackagingWrite,
	"list":            CapPackagingWrite,
	"deprecate":       CapPackagingWrite,
	"delete":          CapPackagingManage,
	"permanentdelete": CapPackagingManage,
	"unpublish":       CapPackagingManage,
	"restoretofeed":   CapPackagingManage,
}

// packageBatchWrite classifies a package batch by its operation: one batch
// route promotes and deletes alike under vso.packaging_write, so the body is
// the only place the difference exists. An operation it cannot read, or does
// not know, is refused.
func packageBatchWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	var batch struct {
		Operation *string `json:"operation"`
	}
	if err := decodeUnique(body, &batch); err != nil {
		return Verdict{}, err
	}
	if batch.Operation == nil {
		return Verdict{}, fmt.Errorf("adoscope: package batch names no operation")
	}
	c, ok := packageBatchOps[strings.ToLower(*batch.Operation)]
	if !ok {
		return Verdict{}, fmt.Errorf("adoscope: package batch operation %q is not one this catalogue classifies", *batch.Operation)
	}
	return Verdict{Capability: c}, nil
}

// pullRequestWrite classifies a pull-request write into pr_create, pr_vote or pr_complete, promoting
// it to CapPolicyBypass when the body asks to complete the PR through the branch policy. Azure DevOps
// takes the same vso.code_write scope for all of them, so the route and the body are the only places
// the difference exists. sub is the segment after the PR id (reviewers, threads, labels, …) and
// subNext2 the segment two after it (statuses in iterations/{n}/statuses).
//
// pr_vote is anything a branch policy can count as a person's review: a write under reviewers, a status, a
// thread resolved, and a sub this catalogue does not list (fail-closed).
func pullRequestWrite(method string, req Request, sub, subNext2 string) (Verdict, error) {
	switch {
	case sub == "reviewers", sub == "statuses", sub == "iterations" && subNext2 == "statuses":
		return Verdict{Capability: CapPRVote}, nil
	case sub == "threads" && (method == http.MethodPatch || method == http.MethodPut):
		return threadWrite(req)
	case sub == "threads", slices.Contains(pullRequestCreateSubs, sub):
		return Verdict{Capability: CapPRCreate}, nil
	case sub != "":
		return Verdict{Capability: CapPRVote}, nil
	case method == http.MethodDelete:
		return Verdict{Capability: CapPRCreate}, nil
	}
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	if len(body) == 0 {
		return Verdict{Capability: CapPRCreate}, nil
	}
	var pr struct {
		Status            json.RawMessage `json:"status"`
		AutoCompleteSetBy json.RawMessage `json:"autoCompleteSetBy"`
		CompletionOptions *struct {
			BypassPolicy *bool `json:"bypassPolicy"`
		} `json:"completionOptions"`
		Reviewers []struct {
			Vote *int `json:"vote"`
		} `json:"reviewers"`
	}
	if err := decodeUnique(body, &pr); err != nil {
		return Verdict{}, err
	}
	// The server's enum parser ORs a comma-joined list (active,abandoned is completed), so a status
	// completes unless it is exactly one that does not.
	keeps := len(pr.Status) == 0 || string(pr.Status) == "null" ||
		enumIs(pr.Status, "notSet", 0) || enumIs(pr.Status, "active", 1) || enumIs(pr.Status, "abandoned", 2)
	completes := !keeps || (len(pr.AutoCompleteSetBy) > 0 && string(pr.AutoCompleteSetBy) != "null") ||
		pr.CompletionOptions != nil
	votes := false
	for _, r := range pr.Reviewers {
		votes = votes || (r.Vote != nil && *r.Vote != 0)
	}
	switch {
	case completes && votes:
		// A bypass is a completion too, so a bypass that votes is refused here as well.
		return Verdict{}, fmt.Errorf("adoscope: a pull-request body that both completes and votes is two requests")
	case pr.CompletionOptions != nil && pr.CompletionOptions.BypassPolicy != nil && *pr.CompletionOptions.BypassPolicy:
		return Verdict{Capability: CapPolicyBypass}, nil
	case completes:
		return Verdict{Capability: CapPRComplete}, nil
	case votes:
		return Verdict{Capability: CapPRVote}, nil
	}
	return Verdict{Capability: CapPRCreate}, nil
}

// pullRequestCreateSubs are the subs under a pull request that are pr_create whatever their body:
// none can count as a person's review. iterations is here for every iteration route but
// iterations/{n}/statuses, which pullRequestWrite has already made pr_vote.
var pullRequestCreateSubs = []string{"labels", "workitems", "properties", "attachments", "conflicts", "share", "iterations"}

// threadWrite is a thread update: resolving it (any status but active) is pr_vote, since a branch
// policy can require every comment resolved; editing it is pr_create. A body this catalogue cannot
// read is pr_vote. A withheld body is still asked for.
func threadWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if errors.Is(err, ErrNeedsBody) {
		return Verdict{}, err
	}
	if err != nil {
		return Verdict{Capability: CapPRVote}, nil
	}
	if len(body) == 0 {
		return Verdict{Capability: CapPRCreate}, nil
	}
	var thread struct {
		Status json.RawMessage `json:"status"`
	}
	if err := decodeUnique(body, &thread); err != nil || (len(thread.Status) > 0 && !enumIs(thread.Status, "active", 1)) {
		return Verdict{Capability: CapPRVote}, nil
	}
	return Verdict{Capability: CapPRCreate}, nil
}

// enumIs reports whether raw is the Azure DevOps enum value name (any case) or its number, as a JSON
// number or a string of digits (the server's enum parser reads both). A list or anything else is not.
func enumIs(raw json.RawMessage, name string, number float64) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s != "" && strings.Trim(s, "0123456789") == "" {
			n, err := strconv.ParseFloat(s, 64)
			return err == nil && n == number
		}
		return strings.EqualFold(s, name)
	}
	var n float64
	return json.Unmarshal(raw, &n) == nil && n == number
}

// refUpdate is the one field of a ref update this catalogue reads.
type refUpdate struct {
	Name string `json:"name"`
}

// refWrite classifies a ref move (Update Refs, or a push): CapCodeWrite, with
// the ref names the body carries, so the caller can hold them to the run's
// branch namespace — a rule of the run, not of Azure DevOps' scopes.
func refWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	refs, err := refNames(body)
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Capability: CapCodeWrite, Refs: refs}, nil
}

// filterRefWrite classifies Update Ref (PATCH refs?filter=heads/<branch>):
// CapCodeWrite on "refs/" + the ONE filter value. A query that does not parse,
// or a missing, empty or repeated filter — in any key spelling, since the
// service reads keys without regard to case — names no ref that can be checked.
func filterRefWrite(req Request) (Verdict, error) {
	q, err := url.ParseQuery(req.RawQuery)
	if err != nil {
		return Verdict{}, ErrNoRef
	}
	var filters []string
	for k, vs := range q {
		if strings.EqualFold(strings.TrimSpace(k), "filter") {
			filters = append(filters, vs...)
		}
	}
	if len(filters) != 1 || strings.TrimSpace(filters[0]) == "" {
		return Verdict{}, ErrNoRef
	}
	return oneRef("refs/" + filters[0])
}

// annotatedTagWrite classifies Annotated Tags – Create: CapCodeWrite on
// refs/tags/<name>, the tag the body's name creates.
func annotatedTagWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	var tag struct {
		Name string `json:"name"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return Verdict{}, ErrNoRef
	}
	if err := decodeUnique(body, &tag); err != nil {
		return Verdict{}, err
	}
	if strings.TrimSpace(tag.Name) == "" {
		return Verdict{}, ErrNoRef
	}
	return oneRef("refs/tags/" + tag.Name)
}

// generatedRefWrite classifies Cherry Picks and Reverts – Create: CapCodeWrite
// on the branch the body's generatedRefName creates.
func generatedRefWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	var op struct {
		GeneratedRefName string `json:"generatedRefName"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return Verdict{}, ErrNoRef
	}
	if err := decodeUnique(body, &op); err != nil {
		return Verdict{}, err
	}
	if strings.TrimSpace(op.GeneratedRefName) == "" {
		return Verdict{}, ErrNoRef
	}
	return oneRef(op.GeneratedRefName)
}

// oneRef is a CapCodeWrite verdict on ref, held to CheckRefName.
func oneRef(ref string) (Verdict, error) {
	if err := CheckRefName(ref); err != nil {
		return Verdict{}, err
	}
	return Verdict{Capability: CapCodeWrite, Refs: []string{ref}}, nil
}

// refNames are the refs a ref-update or push body names. Two endpoint shapes
// are parsed — a bare array of updates, or an object with refUpdates — so one
// ref gate covers both doors. A body naming no ref is refused rather than
// classified: "none visible" is not an answer a policy can apply to.
func refNames(body []byte) ([]string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ErrNoRef
	}
	var updates []refUpdate
	if bytes.TrimSpace(body)[0] == '[' {
		if err := decodeUnique(body, &updates); err != nil {
			return nil, err
		}
	} else {
		var push struct {
			RefUpdates []refUpdate `json:"refUpdates"`
		}
		if err := decodeUnique(body, &push); err != nil {
			return nil, err
		}
		updates = push.RefUpdates
	}
	var out []string
	for _, u := range updates {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			continue
		}
		if err := CheckRefName(name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, ErrNoRef
	}
	return out, nil
}

// CheckRefName refuses a ref name git would not accept, or that a
// run-namespace check could read differently from the service. It is the one
// ref-name rule both Azure DevOps doors apply, so a name one door refuses
// cannot move a ref through the other.
//
//   - ".." is a traversal: "refs/heads/wardyn/<run>/../../main" passes a
//     run-namespace prefix test and names main;
//   - a backslash is a separator to the service but not to a namespace
//     prefix test keyed by the forward-slash spelling;
//   - space, tab, "^", "~", ":", "?", "*" and "[" are forbidden by git's own
//     check-ref-format;
//   - a control byte (LF/CR above all) is how a second command rides a
//     line-oriented reader.
func CheckRefName(ref string) error {
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\\^~:?*[") {
		return fmt.Errorf("refusing malformed refname %q", ref)
	}
	if i := strings.IndexFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("refusing refname %q: control character at byte %d", ref, i)
	}
	return nil
}

// batchOp is the fields of a $batch operation this catalogue reads.
type batchOp struct {
	Method  string            `json:"method"`
	URI     string            `json:"uri"`
	Headers map[string]string `json:"headers"`
}

// batchCapability is the capability a work-item $batch needs: the WIDEST of
// its operations, so one DELETE among edits makes the batch CapWorkAdmin.
//
// It refuses a batch carrying an operation aimed anywhere but the work-item
// area — the batch door forwards each operation's URI internally, so one
// foreign URI would make a work-item capability the capability for a request
// landing in another area entirely — and an operation whose method it cannot
// read: none, one it does not know, or an X-HTTP-Method-Override header in
// any spelling.
func batchCapability(req Request) (Capability, error) {
	body, err := peekBody(req)
	if err != nil {
		return "", err
	}
	var ops []batchOp
	if err := decodeUnique(body, &ops); err != nil {
		return "", err
	}
	if len(ops) == 0 {
		return "", fmt.Errorf("adoscope: a $batch naming no operation cannot be classified")
	}
	out := CapWorkWrite
	for _, op := range ops {
		res, err := batchOpIsWorkItem(op.URI, req.Org)
		if err != nil {
			return "", fmt.Errorf("adoscope: $batch operation %q: %w", op.URI, err)
		}
		method := strings.ToUpper(strings.TrimSpace(op.Method))
		if !knownMethod(method) {
			return "", fmt.Errorf("adoscope: $batch operation %q: method %q is not one this catalogue classifies", op.URI, op.Method)
		}
		for k := range op.Headers {
			if strings.EqualFold(strings.TrimSpace(k), "X-HTTP-Method-Override") {
				return "", fmt.Errorf("adoscope: $batch operation %q carries a method override — which method the server acts on is not knowable", op.URI)
			}
		}
		// An operation's DELETE is CapWorkAdmin whatever it names, a comment
		// included: the batch door is not where a finer split is worth reading.
		if method == http.MethodDelete || slices.Contains(witAdminResources, res) {
			out = CapWorkAdmin
		}
	}
	return out, nil
}

// batchOpIsWorkItem holds one $batch operation URI to the same two rules the
// outer request is held to, and returns the work-item resource it names: under the work-item area, and on the organisation
// the row pinned. Accepted shapes, with _apis at a fixed depth:
//
//	/_apis/wit/…                 organisation-relative
//	/{org}/_apis/wit/…           the pinned organisation named
//	/{org}/{project}/_apis/wit/… the pinned organisation named, then a project
//
// Any other first segment is refused, including the project-relative
// "/{project}/_apis/wit/…" shape Microsoft's WIT reference also shows:
// whether the batch door resolves that segment as a project or an
// organisation is unverified, so it stays refused until a live
// two-organisation probe settles it. An absolute URI is refused outright,
// since the batch door is not a place to re-run host admission.
func batchOpIsWorkItem(uri, org string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "", fmt.Errorf("an operation URI is relative to the organisation, never absolute")
	}
	segs, err := decodeSegments(u.Path)
	if err != nil {
		return "", err
	}
	if len(segs) > 0 && segs[0] != "_apis" {
		if !strings.EqualFold(segs[0], strings.TrimSpace(org)) {
			return "", fmt.Errorf("names organisation %q, row pins %q", segs[0], org)
		}
		segs = segs[1:]
	}
	i := slices.Index(segs, "_apis")
	if i < 0 || i > 1 || i+1 >= len(segs) || segs[i+1] != "wit" {
		return "", fmt.Errorf("is not a work-item URL")
	}
	// The resource is the segment after _apis/wit/, "" when there is none.
	if i+2 < len(segs) {
		return foldSegment(segs[i+2]), nil
	}
	return "", nil
}
