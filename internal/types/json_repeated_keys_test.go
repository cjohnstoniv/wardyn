// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestRefuseRepeatedJSONKeys: a key written twice in one object is refused at any depth, named by
// its path, also when one spelling escapes a letter; the same key in two objects, and a number past
// float64's range, are fine.
func TestRefuseRepeatedJSONKeys(t *testing.T) {
	for src, want := range map[string]string{
		`{"a":1,"a":2}`: `the body has the key "a" twice`,
		`{"spec":{"denied_domains":[],"denied_domains":["x"]}}`: `the body.spec has the key "denied_domains" twice`,
		`[{"n":1},{"n":1,"n":2}]`:                               `the body[1] has the key "n" twice`,
		`{"a":1,"\u0061":2}`:                                    `the body has the key "a" twice`,
		"{\"a\xff\":1,\"a\xfe\":2}":                             "the body has the key \"a\uFFFD\" twice",
		manyKeys(5000, `"k7":2`):                                `the body has the key "k7" twice`,
		manyKeys(5000, `"n":{"x":1,"x":2},"k7":2`):              `the body.n has the key "x" twice`,
		manyKeys(5000, `"k7":{"x":1,"x":2}`):                    `the body has the key "k7" twice`,
	} {
		if err := RefuseRepeatedJSONKeys([]byte(src), "the body"); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	for _, src := range []string{`{"a":{"n":1},"b":{"n":1}}`, `{"big":1e400}`, `[]`, `"text"`, `null`} {
		if err := RefuseRepeatedJSONKeys([]byte(src), "the body"); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}

// manyKeys is an object of n distinct keys k0, k1, ... and then more: past a few keys an object's
// keys are compared when it ends, and the first refusal in the body is still the one named.
func manyKeys(n int, more string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		fmt.Fprintf(&b, `"k%d":1,`, i)
	}
	return b.String() + more + "}"
}

// wallsBody is the shape of a walls PUT: a policy under "spec".
type wallsBody struct {
	Spec RunPolicySpec `json:"spec"`
}

// TestRefuseRepeatedJSONKeysForAField: encoding/json reads a key into the field of that name or,
// failing that, one it equals ignoring case (Unicode folding), keeping the last of two spellings and
// merging two objects. Two keys that land in one field are refused at any depth; a map's keys, and
// the keys inside an untyped value, are compared exactly; a lone key in another case is the decode's.
func TestRefuseRepeatedJSONKeysForAField(t *testing.T) {
	type key struct {
		Key string `json:"key"`
	}
	type embedded struct {
		A int `json:"a"`
	}
	type promoted struct {
		embedded
		B int `json:"b"`
	}
	type typed struct {
		List  []key           `json:"list"`
		Map   map[string]key  `json:"map"`
		Promo promoted        `json:"promo"`
		Raw   json.RawMessage `json:"raw"`
		Any   any             `json:"any"`
		Skip  string          `json:"-"`
		Free  map[string]int  `json:"free"`
		Pair  struct {
			A int `json:"a"`
			B int `json:"A"`
		} `json:"pair"`
	}
	for src, want := range map[string]string{
		`{"spec":{"denied_domains":["x.example"],"Denied_Domains":[]}}`: `the body.spec has the key "denied_domains" twice, written "denied_domains" and "Denied_Domains"`,
		`{"spec":{"denied_domains":["x.example"],"DENIED_DOMAINſ":[]}}`: `the body.spec has the key "denied_domains" twice, written "denied_domains" and "DENIED_DOMAINſ"`,
		`{"spec":{"Denied_Domains":[],"DENIED_DOMAINS":["x.example"]}}`: `the body.spec has the key "denied_domains" twice, written "Denied_Domains" and "DENIED_DOMAINS"`,
		`{"spec":{"denied_domains":["x.example"]},"SPEC":{}}`:           `the body has the key "spec" twice, written "spec" and "SPEC"`,
	} {
		if err := RefuseRepeatedJSONKeysFor([]byte(src), "the body", &wallsBody{}); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
		// The defect: the scan without the type passes it, and the decode keeps one spelling.
		if err := RefuseRepeatedJSONKeys([]byte(src), "the body"); err != nil {
			t.Errorf("%s: untyped scan: %v", src, err)
		}
	}
	for src, want := range map[string]string{
		`{"list":[{"key":"a"},{"key":"a","Key":"b"}]}`: "the body.list[1] has the key \"key\" twice, written \"key\" and \"\u212aey\"",
		`{"map":{"x":{"key":"a","KEY":"b"}}}`:          `the body.map.x has the key "key" twice, written "key" and "KEY"`,
		`{"promo":{"a":1,"A":2}}`:                      `the body.promo has the key "a" twice, written "a" and "A"`,
		`{"raw":{"n":1,"n":2}}`:                        `the body.raw has the key "n" twice`,
	} {
		if err := RefuseRepeatedJSONKeysFor([]byte(src), "the body", &typed{}); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	for _, src := range []string{
		`{"spec":{"Denied_Domains":["x.example"]}}`,                // one spelling: read as the decode reads it
		`{"free":{"README":1,"readme":2}}`,                         // a map's keys are its own data
		`{"raw":{"n":1,"N":2},"any":{"n":1,"N":2}}`,                // untyped values: exact keys only
		`{"Skip":"a","skip":"b"}`,                                  // a "-" field takes no key
		`{"pair":{"a":1,"A":2}}`,                                   // two fields, each takes its own name
		`{"list":[{"key":"a"},{"KEY":"b"}],"map":{"k":{},"K":{}}}`, // one spelling per object
	} {
		var dst any = &typed{}
		if strings.Contains(src, "spec") {
			dst = &wallsBody{}
		}
		if err := RefuseRepeatedJSONKeysFor([]byte(src), "the body", dst); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	// The field set is encoding/json's: a file path keyed map keeps README and readme both.
	var gen ImageBuildGenerator
	if err := DecodeOneJSON(strings.NewReader(`{"salt":"s","files_sha256":{"README":"a","readme":"b"}}`), &gen, true); err != nil ||
		gen.FilesSHA256["README"] != "a" || gen.FilesSHA256["readme"] != "b" {
		t.Errorf("files_sha256 = %v, err %v; want both paths", gen.FilesSHA256, err)
	}
}

// TestJSONFieldsAreEncodingJSONs: the names a struct's keys land in are the ones encoding/json
// writes for it (embedded promotion, tags, "-", omitempty, string, a name at two depths and a tie
// that cancels out), and folding matches strings.EqualFold, which is the fold encoding/json
// documents. A tag name encoding/json does not accept is left out: what it does with one depends on
// the Go release, and no struct here has one.
func TestJSONFieldsAreEncodingJSONs(t *testing.T) {
	type Inner struct {
		A   int `json:"a"`
		X   int
		Dup int `json:"dup"`
		Top int `json:"top"`
	}
	type other struct {
		B   int
		Dup int `json:"dup"`
	}
	type Deep struct{ Inner }
	type outer struct {
		Inner
		*other
		Deep
		Tagged Inner `json:"tagged"`
		Skip   int   `json:"-"`
		Dash   int   `json:"-,"`
		Omit   int   `json:"omit,omitempty"`
		Str    int   `json:",string"`
		Top    int
		hidden int
	}
	v := outer{Inner: Inner{1, 1, 1, 1}, other: &other{1, 1}, Deep: Deep{Inner{1, 1, 1, 1}}, Tagged: Inner{1, 1, 1, 1},
		Skip: 1, Dash: 1, Omit: 1, Str: 1, Top: 1, hidden: 1}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]json.RawMessage
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(maps.Keys(written))
	got := slices.Sorted(maps.Keys(jsonFieldsOf(reflect.TypeOf(v)).exact))
	if !slices.Equal(got, want) {
		t.Errorf("fields = %v, encoding/json writes %v", got, want)
	}
	names := []string{"s", "S", "ſ", "k", "K", "K", "ß", "ẞ", "SS", "a", "Å", "å", "Å", "ǅ", "ǆ", "Ǆ", "i", "İ", "ı"}
	for _, a := range names {
		for _, b := range names {
			if (foldJSONName(a) == foldJSONName(b)) != strings.EqualFold(a, b) {
				t.Errorf("fold(%q) == fold(%q) is %v, strings.EqualFold says %v", a, b, !strings.EqualFold(a, b), strings.EqualFold(a, b))
			}
		}
	}
}

// TestDecodeOneJSON: one value, then the end of the input; what a json.Decoder lets through is
// refused, and its own answers (io.EOF for an empty body, a read error as it is) are kept.
func TestDecodeOneJSON(t *testing.T) {
	type body struct {
		A int `json:"a"`
	}
	for src, want := range map[string]string{
		`{"a":1} {"a":2}`: "more than one JSON value",
		`{"a":1} junk`:    "invalid character 'j' looking for beginning of value",
		`{"a":1,"a":2}`:   `the body has the key "a" twice`,
		`{"a":1,"A":2}`:   `the body has the key "a" twice, written "a" and "A"`,
		`{"a":1,"zz":2}`:  `json: unknown field "zz"`,
		`{"a":`:           "unexpected EOF",
		` `:               "EOF",
		"tru ":            "invalid character ' ' in literal true (expecting 'e')",
	} {
		var dst body
		if err := DecodeOneJSON(strings.NewReader(src), &dst, true); err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", src, err, want)
		}
	}
	var dst body
	if err := DecodeOneJSON(strings.NewReader(""), &dst, false); err != io.EOF {
		t.Errorf("empty body: %v, want io.EOF", err)
	}
	if err := DecodeOneJSON(strings.NewReader("{\"a\":2,\"zz\":1}\n\n"), &dst, false); err != nil || dst.A != 2 {
		t.Errorf("unknown field, not strict: a = %d, err %v", dst.A, err)
	}
	over := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader(`{"a":1}`+strings.Repeat(" ", 64)+`{}`)), 32)
	var maxErr *http.MaxBytesError
	if err := DecodeOneJSON(over, &dst, true); !errors.As(err, &maxErr) {
		t.Errorf("over the cap: %v, want *http.MaxBytesError", err)
	}
	if got := JSONFieldName(&dst, "A"); got != "a" {
		t.Errorf("JSONFieldName(A) = %q, want a", got)
	}
	if got := JSONFieldName(&dst, "zz"); got != "zz" {
		t.Errorf("JSONFieldName(zz) = %q, want zz", got)
	}
}

// TestDecodeOneJSONRows: the rows of an array, dst itself or the field rows names (in any case the
// decode reads it in), are counted before anything is decoded, so a megabyte of empty rows over
// the cap is refused without decoding a row.
func TestDecodeOneJSONRows(t *testing.T) {
	type batch struct {
		Events []AuditEvent `json:"events"`
	}
	var rows []FederatedAuditEvent
	var b batch
	for _, c := range []struct {
		src  string
		dst  any
		rows string
	}{{`[1,2,3]`, &rows, ""}, {`{"events":[1,2,3]}`, &b, "events"}, {`{"Events":[{},{},{}]}`, &b, "events"}} {
		var tooMany *TooManyJSONRowsError
		if err := DecodeOneJSONRows(strings.NewReader(c.src), c.dst, true, c.rows, 2); !errors.As(err, &tooMany) || tooMany.Rows != 3 || tooMany.Max != 2 {
			t.Errorf("%s: err = %v, want 3 rows over 2", c.src, err)
		}
	}
	if err := DecodeOneJSONRows(strings.NewReader(`{"events":[{"data":[1,2,3]},{}]}`), &b, true, "events", 2); err != nil || len(b.Events) != 2 {
		t.Errorf("two events = %v, %d; want both read", err, len(b.Events))
	}
	if err := DecodeOneJSONRows(strings.NewReader(`[1,2,3] {}`), &rows, true, "", 2); err == nil || err.Error() != "more than one JSON value" {
		t.Errorf("a second value after too many rows: %v, want the second value named first", err)
	}
	empty := "[" + strings.TrimSuffix(strings.Repeat(`{},`, 350000), ",") + "]"
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := DecodeOneJSONRows(strings.NewReader(empty), &rows, true, "", 500)
	runtime.ReadMemStats(&after)
	var tooMany *TooManyJSONRowsError
	if !errors.As(err, &tooMany) || tooMany.Rows != 350000 || after.TotalAlloc-before.TotalAlloc > 32<<20 {
		t.Errorf("1 MiB of empty rows: %v, %d MiB allocated; want refused within 32 MiB", err, (after.TotalAlloc-before.TotalAlloc)>>20)
	}
}
