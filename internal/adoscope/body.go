// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// MaxBodyPeek bounds what the classifier will look at. Three routes need the
// body — a pull-request completion, a ref move and a work-item batch — and all
// three are small requests; 256 KiB is far above any of them and far below
// anything worth buffering on a governed path. A body that does not fit is
// REFUSED, never classified on its visible prefix.
const MaxBodyPeek = 256 << 10

// maxJSONDepth bounds the duplicate-key walk. A 256 KiB body of nothing but
// "[" is legal JSON and would recurse 256k deep; the real bodies are three or
// four levels.
const maxJSONDepth = 64

// peekBody returns the body bytes the classifier may trust, or refuses.
//
// EVERY refusal here is a case where the bytes on the wire are not the bytes
// the server will parse, or are not all of them:
//   - a Content-Encoding means the peek holds compressed bytes, and a
//     bypassPolicy flag inside a gzip stream is invisible to a substring or a
//     JSON decode alike;
//   - a declared length longer than the peek, or a peek sitting exactly on the
//     bound, means the body continues past what we can see — and the flag that
//     matters may be in the part we cannot.
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
// from here. The lookup is headerValues', so a non-canonical key on the map
// cannot hide a Content-Encoding any more than it can hide a method override.
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
// This is not pedantry about JSON. Azure DevOps' parser is last-key-wins, and
// so is Go's, but they do not have to agree about which key is last if either
// ever changes — and a body of
//
//	{"completionOptions":{"bypassPolicy":false,…,"bypassPolicy":true}}
//
// is written precisely so that a classifier reading the first occurrence sees
// "false" while the server acts on "true". Refusing the body is the only
// answer that cannot be gamed by the order the keys arrive in.
//
// The comparison FOLDS CASE, because the server's binding does: "bypassPolicy"
// and "BYPASSPOLICY" are one property to Azure DevOps, so a body carrying both
// is the same trick spelled differently.
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
	// Trailing content after the first value is a second document the server
	// may read differently from us.
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
	// The closing delimiter of this array (an object's is consumed by
	// walkObject). A malformed stream surfaces as the decode error above on
	// the next read, so the token is taken and not inspected.
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
		folded := strings.ToLower(name)
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

// pullRequestWrite classifies a pull-request write, promoting it to
// CapPolicyBypass when the body asks to complete the PR THROUGH the branch
// policy. Azure DevOps takes the same vso.code_write scope either way, so the
// body is the only place the difference exists.
func pullRequestWrite(method string, req Request) (Verdict, error) {
	if method == http.MethodDelete {
		return Verdict{Capability: CapPR}, nil
	}
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	if len(body) == 0 {
		return Verdict{Capability: CapPR}, nil
	}
	var pr struct {
		CompletionOptions *struct {
			BypassPolicy *bool `json:"bypassPolicy"`
		} `json:"completionOptions"`
	}
	if err := decodeUnique(body, &pr); err != nil {
		return Verdict{}, err
	}
	if pr.CompletionOptions != nil && pr.CompletionOptions.BypassPolicy != nil && *pr.CompletionOptions.BypassPolicy {
		return Verdict{Capability: CapPolicyBypass}, nil
	}
	return Verdict{Capability: CapPR}, nil
}

// refUpdate is the one field of a ref update this catalogue reads.
type refUpdate struct {
	Name string `json:"name"`
}

// refWrite classifies a ref move, which is CapCodeWrite on an ordinary branch
// and CapPolicyBypass on a policy-protected one — a distinction Azure DevOps
// does not make in its scopes, and the reason the ref names are parsed here
// rather than left to a later gate.
//
// The names are returned on EVERY verdict, protected or not: the caller keeps
// the per-run protected-branch cache, and handing it the refs lets it re-decide
// without re-parsing a body it no longer has.
func refWrite(req Request) (Verdict, error) {
	body, err := peekBody(req)
	if err != nil {
		return Verdict{}, err
	}
	refs, err := refNames(body)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Capability: CapCodeWrite, Refs: refs}
	if req.RefProtected != nil && slices.ContainsFunc(refs, req.RefProtected) {
		v.Capability = CapPolicyBypass
	}
	return v, nil
}

// refNames are the refs a ref-update or push body names. The two endpoints
// carry two shapes — the refs endpoint takes a bare ARRAY of updates, a push
// takes an object with refUpdates — and both are parsed here so that one ref
// gate covers both doors.
//
// A body naming no ref is REFUSED rather than classified: the whole point of
// reading it is to learn which branches move, and "none visible" is not an
// answer a policy can be applied to.
func refNames(body []byte) ([]string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("adoscope: a ref update with no body names no branch to gate")
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
		return nil, fmt.Errorf("adoscope: a ref update naming no branch cannot be gated")
	}
	return out, nil
}

// CheckRefName refuses a ref name that is not one git would accept as it
// reads, or that a protected-ref check could read differently from the
// service. It is the ONE ref-name rule both Azure DevOps doors apply — the REST
// refs/pushes body here and the git broker's receive-pack command — so a name
// one door refuses cannot move a ref through the other.
//
//   - ".." is a traversal: "refs/heads/wardyn/<run>/../../main" passes a
//     run-namespace prefix test and names main;
//   - a backslash is a separator to the service but not to a protected-branch
//     cache keyed by the forward-slash spelling;
//   - space, tab, "^", "~", ":", "?", "*" and "[" are forbidden by git's own
//     check-ref-format, so refusing them costs no legitimate ref anything;
//   - a control byte (an LF or CR above all) is how a second command rides a
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

// batchOp is the one field of a $batch operation this catalogue reads.
type batchOp struct {
	URI string `json:"uri"`
}

// batchIsWorkItemsOnly refuses a work-item $batch that carries an operation
// aimed anywhere but the work-item area.
//
// The batch door forwards each operation's URI internally, so a batch is only
// worth CapWorkWrite if every operation in it IS a work-item write. One
// foreign URI would make CapWorkWrite the capability for a request that lands
// in another area entirely — a tunnel with a work-item label.
func batchIsWorkItemsOnly(req Request) error {
	body, err := peekBody(req)
	if err != nil {
		return err
	}
	var ops []batchOp
	if err := decodeUnique(body, &ops); err != nil {
		return err
	}
	if len(ops) == 0 {
		return fmt.Errorf("adoscope: a $batch naming no operation cannot be classified")
	}
	for _, op := range ops {
		if err := batchOpIsWorkItem(op.URI, req.Org); err != nil {
			return fmt.Errorf("adoscope: $batch operation %q: %w", op.URI, err)
		}
	}
	return nil
}

// batchOpIsWorkItem holds ONE $batch operation URI to the same two rules the
// outer request is held to: it is under the work-item area, and it is on the
// organisation the row pinned.
//
// An operation URI is resolved by the batch door RELATIVE TO THE ORGANISATION
// the batch was POSTed to — Microsoft's own examples write both
// "/_apis/wit/workItems/284" and the project-relative
// "/Fabrikam-Fiber-Git/_apis/wit/workItems/$Task" (WIT Batch, TFS REST API
// reference). So the accepted shapes are positional, with _apis at a fixed
// depth:
//
//	/_apis/wit/…                 organisation-relative
//	/{project}/_apis/wit/…       project-relative, under the pinned organisation
//	/{org}/_apis/wit/…           the pinned organisation named (same shape)
//	/{org}/{project}/_apis/wit/… the pinned organisation named, then a project
//
// A first segment that is not the pinned organisation is a PROJECT only where
// _apis follows it directly; with a second segment before _apis it can only be
// an organisation, and another organisation is refused — that was the hole: a
// batch POSTed to the pinned organisation carrying writes into another one
// under a work_write the row had granted. _apis anywhere deeper is refused.
//
// An ABSOLUTE URI is refused outright: it could name another host or another
// service entirely, and the batch door is not a place to re-run host
// admission. The segment decode is the outer request's, so a dot segment or a
// hidden separator inside an operation URI is refused here too.
func batchOpIsWorkItem(uri, org string) error {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return fmt.Errorf("an operation URI is relative to the organisation, never absolute")
	}
	segs, err := decodeSegments(u.Path)
	if err != nil {
		return err
	}
	i := slices.Index(segs, "_apis")
	switch {
	case i == 2 && segs[0] != strings.ToLower(strings.TrimSpace(org)):
		return fmt.Errorf("names organisation %q, row pins %q", segs[0], org)
	case i < 0 || i > 2 || i+1 >= len(segs) || segs[i+1] != "wit":
		return fmt.Errorf("is not a work-item URL")
	}
	return nil
}
