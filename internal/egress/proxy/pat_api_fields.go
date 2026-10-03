// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// The fields a forge API request names, read from every place a forge reads
// them: the query string and a JSON, form-urlencoded or multipart body. A
// path-only check passes every same-repository request while a create request's
// body retargets another project, so these fields are part of every row.

// patAPITargetKeys name a project, repository or ref outside the path, or turn
// on a method. Any of them, present anywhere with any value, refuses the request:
// a numeric project id cannot be mapped to a granted path, and "_method" is how
// a Rack or Rails app reads a method override from a form body.
var patAPITargetKeys = map[string]bool{
	"target_project_id": true, "source_project_id": true, "from_project_id": true, "project_id": true,
	"fromrepo": true, "_method": true,
}

// patAPIAutoMergeKeys turn on a merge that happens later, without a merge
// request. Present at all (a create, or an update), they refuse the request.
var patAPIAutoMergeKeys = map[string]bool{
	"auto_merge": true, "automerge": true, "merge_when_pipeline_succeeds": true,
	"merge_when_checks_succeed": true, "auto_merge_strategy": true,
}

// patAPIMaxParts bounds a multipart body's parts.
const patAPIMaxParts = 256

// patAPIFields is what a request names. keys holds every field name seen at any
// depth, lower-cased; a Rails-style a[b] contributes a, b and a[b]. vals holds
// the string value of each top-level field, repeated fields in order. obj is the
// decoded top-level JSON object, nil for any other body.
type patAPIFields struct {
	keys map[string]bool
	vals map[string][]string
	obj  map[string]any
}

func (f *patAPIFields) add(name, val string) {
	lower := strings.ToLower(name)
	f.keys[lower] = true
	for _, part := range strings.FieldsFunc(lower, func(c rune) bool { return c == '[' || c == ']' }) {
		f.keys[part] = true
	}
	f.vals[lower] = append(f.vals[lower], val)
}

// collectPATAPIFields reads the query and the body. A body it cannot read whole
// and parse is refused, as the Azure DevOps gate refuses one: an encoded body, a
// body over the peek cap, a content type it does not parse, a duplicate JSON key.
// Every method's body is read, since a framework may read a GET's body params.
func collectPATAPIFields(r *http.Request) (*patAPIFields, string) {
	f := &patAPIFields{keys: map[string]bool{}, vals: map[string][]string{}}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, "the query string cannot be parsed"
	}
	for name, vals := range q {
		for _, v := range vals {
			f.add(name, v)
		}
	}
	body, why := peekBody(r, adoscope.MaxBodyPeek)
	if why != "" {
		return nil, why
	}
	if len(body) == 0 {
		return f, ""
	}
	// Two Content-Type headers are read as one or the other by the parser on each side.
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || len(r.Header.Values("Content-Type")) > 1 {
		return nil, "its body has no content type this door parses"
	}
	switch mt {
	case "application/json":
		err = f.addJSON(body)
	case "application/x-www-form-urlencoded":
		var form url.Values
		if form, err = url.ParseQuery(string(body)); err == nil {
			for name, vals := range form {
				for _, v := range vals {
					f.add(name, v)
				}
			}
		}
	case "multipart/form-data":
		err = f.addMultipart(body, params["boundary"])
	default:
		return nil, "its body is a " + mt + ", which this door does not parse"
	}
	if err != nil {
		return nil, "its body cannot be parsed: " + err.Error()
	}
	return f, ""
}

func (f *patAPIFields) addJSON(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := walkJSONKeys(dec, 0, func(k string) { f.add(k, "") }); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	if err := json.Unmarshal(body, &f.obj); err != nil || f.obj == nil {
		return errors.New("the body is not a JSON object")
	}
	// walkJSONKeys added every key with an empty value; the top-level values are
	// the ones a row reads.
	for k := range f.obj {
		delete(f.vals, strings.ToLower(k))
	}
	for k, v := range f.obj {
		switch t := v.(type) {
		case string:
			f.vals[strings.ToLower(k)] = []string{t}
		case nil, map[string]any, []any:
			f.vals[strings.ToLower(k)] = []string{""}
		default:
			f.vals[strings.ToLower(k)] = []string{fmt.Sprint(t)}
		}
	}
	return nil
}

// walkJSONKeys visits every object key at any depth and refuses a key repeated
// in one object: the parsers on either side may keep the first or the last.
func walkJSONKeys(dec *json.Decoder, depth int, visit func(string)) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth > 32 {
		return errors.New("the JSON is nested too deeply")
	}
	seen := map[string]bool{}
	for dec.More() {
		if d == '{' {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("the key %q appears twice", k)
			}
			seen[k] = true
			visit(k)
		}
		if err := walkJSONKeys(dec, depth+1, visit); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func (f *patAPIFields) addMultipart(body []byte, boundary string) error {
	if boundary == "" {
		return errors.New("no multipart boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if i >= patAPIMaxParts {
			return errors.New("too many parts")
		}
		val, err := io.ReadAll(io.LimitReader(part, adoscope.MaxBodyPeek))
		if err != nil {
			return err
		}
		if part.FileName() != "" {
			val = nil // a file's bytes are not a field a row reads; its field name still is
		}
		f.add(part.FormName(), string(val))
	}
}

// refusedField names the first request field no row admits, "" when none.
func (f *patAPIFields) refusedField() string {
	for k := range patAPITargetKeys {
		if f.keys[k] {
			return k
		}
	}
	for k := range patAPIAutoMergeKeys {
		if f.keys[k] {
			return k
		}
	}
	return ""
}
