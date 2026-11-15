// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// RefuseRepeatedJSONKeys refuses a JSON value with an object that has a key written twice, at any
// depth, and names that object by its path from root. Decoding keeps only the last of a repeated
// key, so `"denied_domains":["x"],"denied_domains":[]` would read as the empty list. data is one
// value a decoder has already read, so its syntax and its depth are already checked.
func RefuseRepeatedJSONKeys(data []byte, root string) error {
	return RefuseRepeatedJSONKeysFor(data, root, nil)
}

// RefuseRepeatedJSONKeysFor is RefuseRepeatedJSONKeys for a value decoded into dst. encoding/json
// reads an object key into the struct field of that name, or else into a field whose name it equals
// ignoring case (Unicode folding, so "DENIED_DOMAINſ" is one), so "denied_domains" and
// "Denied_Domains" land in one field, and the decode keeps the last (or, for an object, merges the
// two). Two keys that land in one field are refused. A map's keys are its own data and are compared
// exactly, as are the keys inside any value dst does not type (an interface, a json.RawMessage). A
// key in another case on its own is left to the decode, which reads it.
func RefuseRepeatedJSONKeysFor(data []byte, root string, dst any) error {
	s := keyScan{data: data, root: root}
	if err := s.value(reflect.TypeOf(dst), 0, false); err != nil {
		if errors.Is(err, errNotScanned) {
			return fmt.Errorf("%s: %w", root, err)
		}
		return err
	}
	return nil
}

// DecodeOneJSON decodes one JSON value from r into dst. It refuses a second value or other text
// after it, and two keys that land in one field (RefuseRepeatedJSONKeysFor, root "the body"), which
// a json.Decoder lets through. Its other errors are the decoder's: an empty input is io.EOF, and a
// read error such as *http.MaxBytesError comes back as it is. disallowUnknownFields is
// json.Decoder.DisallowUnknownFields.
func DecodeOneJSON(r io.Reader, dst any, disallowUnknownFields bool) error {
	return DecodeOneJSONRows(r, dst, disallowUnknownFields, "", 0)
}

// TooManyJSONRowsError is DecodeOneJSONRows's refusal of an array longer than its cap.
type TooManyJSONRowsError struct {
	Rows, Max int
}

func (e *TooManyJSONRowsError) Error() string {
	return fmt.Sprintf("%d rows, more than %d", e.Rows, e.Max)
}

// DecodeOneJSONRows is DecodeOneJSON with a cap on the rows of one array: dst itself when rows is
// "", else the field of dst's struct whose JSON name is rows. A longer array is refused with
// *TooManyJSONRowsError before anything is decoded, so a body of many empty rows costs its bytes,
// not a decoded value per row. maxRows 0 is no cap.
func DecodeOneJSONRows(r io.Reader, dst any, disallowUnknownFields bool, rows string, maxRows int) error {
	var buf bytes.Buffer // grows by doubling, where io.ReadAll copies a large body several times
	if _, err := buf.ReadFrom(r); err != nil {
		return err
	}
	data := buf.Bytes()
	t := reflect.TypeOf(dst)
	s := keyScan{data: data, root: "the body", rowsAtRoot: rows == ""}
	if rows != "" {
		s.rowsField = jsonFieldsOf(derefType(t)).exact[rows]
	}
	err := s.value(t, 0, s.rowsAtRoot)
	end := s.pos
	if s.skipSpace(); err == nil && s.pos < len(data) {
		err = errMoreThanOneValue
	}
	if err == nil && maxRows > 0 && s.rows > maxRows {
		err = &TooManyJSONRowsError{Rows: s.rows, Max: maxRows}
	}
	if err != nil {
		return firstJSONValueError(data, err)
	}
	// The whole body, with the space after the value: a value cut short then reads as the decoder
	// answers it (`tru ` is an invalid character, not an unexpected end).
	typed := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknownFields {
		typed.DisallowUnknownFields()
	}
	if err := typed.Decode(dst); err != nil {
		return err
	}
	if typed.InputOffset() != int64(end) { // the scan read a value the decoder did not
		return firstJSONValueError(data, errNotScanned)
	}
	return nil
}

var errMoreThanOneValue = errors.New("more than one JSON value")

// firstJSONValueError answers a body the key scan refused or could not read as a json.Decoder
// would, before the scan's own refusal: the first value's syntax and depth, then a second value
// (without this, `{} {"spec":...}` reads as an empty document, and a whole-document PUT replaces
// what was stored with it). The scan reads well-formed JSON only, so its refusal stands only when
// the decoder finds one value.
func firstJSONValueError(data []byte, scanErr error) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(new(json.RawMessage)); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return errMoreThanOneValue
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return scanErr
}

// errNotScanned is the key scan's answer to bytes that are not one well-formed JSON value, or that
// nest deeper than encoding/json reads (maxJSONDepth).
var errNotScanned = errors.New("not one well-formed JSON value")

// maxJSONDepth is encoding/json's nesting limit: a deeper value is refused by the decoder.
const maxJSONDepth = 10000

// keyScanSeed hashes the keys of a large object (keySet).
var keyScanSeed = maphash.MakeSeed()

// keyScan reads one JSON value's keys and skips every other byte without building a value, so the
// check costs a pass over the body and a set of keys per object, never a token per value. It
// trusts the syntax only as far as it reads it: on anything else it stops with errNotScanned, and
// DecodeOneJSONRows hands the body to a json.Decoder, which answers it.
type keyScan struct {
	data       []byte
	pos        int
	root       string
	path       []keyStep // the steps down to the value being read, joined only for a refusal
	sets       []*keySet // one per depth, reused by the objects at that depth
	fold       []byte    // scratch for a key's folded form
	arena      []byte    // the keys that had to be unquoted
	rowsAtRoot bool      // count the rows of the array dst is
	rowsField  *jsonField
	rows       int
}

// keyStep is one step of a path: a member's key, or an element's index.
type keyStep struct {
	key   []byte
	index int
	isKey bool
}

// landedKey is a struct field and the key that set it.
type landedKey struct {
	field *jsonField
	key   []byte
}

// keySet is the keys one object has had so far. A few are kept and compared one by one. Past
// linearKeys only their hashes are kept, and compared when the object ends (repeated): an object
// of a million distinct keys then costs a hash per key and a sort of the hashes, not a lookup per
// key in a table too large for the CPU's caches.
type keySet struct {
	keys   [][]byte
	hashes []uint64
	big    bool
	landed []landedKey
}

const linearKeys = 16

func (k *keySet) reset() {
	k.keys, k.hashes, k.big, k.landed = k.keys[:0], k.hashes[:0], false, k.landed[:0]
}

// add records key and reports whether the object already had it, while the set is small.
func (k *keySet) add(key []byte) bool {
	if k.big {
		if len(k.hashes) == cap(k.hashes) {
			k.hashes = slices.Grow(k.hashes, len(k.hashes)) // doubling: append grows a large slice by less
		}
		k.hashes = append(k.hashes, maphash.Bytes(keyScanSeed, key))
		return false
	}
	for _, e := range k.keys {
		if bytes.Equal(e, key) {
			return true
		}
	}
	if k.keys = append(k.keys, key); len(k.keys) > linearKeys {
		k.big = true
		for _, e := range k.keys {
			k.hashes = append(k.hashes, maphash.Bytes(keyScanSeed, e))
		}
	}
	return false
}

// repeated reports whether two of a big set's hashes are equal, which two keys may share. A few
// thousand are sorted. More are put in order by their top byte and then the next by two counting
// sorts, which leave runs of a few hashes each (the hash is seeded when the process starts, so a
// body cannot choose them), each sorted on its own.
func (k *keySet) repeated() bool {
	hs := k.hashes
	if len(hs) > 1<<12 {
		tmp := make([]uint64, len(hs))
		ends := byteSort(hs, tmp, 56)
		for b := range 256 {
			byteSort(tmp[ends[b]:ends[b+1]], hs[ends[b]:ends[b+1]], 48)
		}
	}
	for i := 0; i < len(hs); {
		j := i + 1
		for j < len(hs) && (len(hs) <= 1<<12 || hs[j]>>48 == hs[i]>>48) {
			j++
		}
		run := hs[i:j]
		slices.Sort(run)
		for n := 1; n < len(run); n++ {
			if run[n] == run[n-1] {
				return true
			}
		}
		i = j
	}
	return false
}

// byteSort puts src into dst in order by the byte at bit shift (a counting sort), and returns
// where each byte's values start in dst, and their end.
func byteSort(src, dst []uint64, shift uint) (ends [257]int) {
	for _, h := range src {
		ends[(h>>shift)&0xff+1]++
	}
	for i := 1; i < len(ends); i++ {
		ends[i] += ends[i-1]
	}
	next := ends
	for _, h := range src {
		b := (h >> shift) & 0xff
		dst[next[b]] = h
		next[b]++
	}
	return ends
}

// refuse names the object being read by its path from root.
func (s *keyScan) refuse(format string, args ...any) error {
	var b strings.Builder
	b.WriteString(s.root)
	for _, st := range s.path {
		if st.isKey {
			b.WriteString(".")
			b.Write(st.key)
		} else {
			b.WriteString("[" + strconv.Itoa(st.index) + "]")
		}
	}
	return fmt.Errorf("%s "+format, append([]any{b.String()}, args...)...)
}

func (s *keyScan) skipSpace() {
	if s.pos < len(s.data) && s.data[s.pos] > ' ' {
		return
	}
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *keyScan) peek() byte {
	if s.pos < len(s.data) {
		return s.data[s.pos]
	}
	return 0
}

// value reads one value typed by t (nil where it is not typed) at depth containers down.
// countRows counts the elements when the value is an array.
func (s *keyScan) value(t reflect.Type, depth int, countRows bool) error {
	s.skipSpace()
	switch s.peek() {
	case '{':
		return s.object(derefType(t), depth+1)
	case '[':
		return s.array(derefType(t), depth+1, countRows)
	case '"':
		return s.skipString()
	case 0, ',', ':', ']', '}':
		return errNotScanned
	}
	start := s.pos // a number, true, false or null, read as far as it follows the grammar
	switch s.data[s.pos] {
	case 't':
		s.word("true")
	case 'f':
		s.word("false")
	case 'n':
		s.word("null")
	default:
		s.number()
	}
	if s.pos == start {
		return errNotScanned
	}
	return nil
}

// word moves past w, or as far as the body follows it.
func (s *keyScan) word(w string) {
	for i := 0; i < len(w) && s.peek() == w[i]; i++ {
		s.pos++
	}
}

// number moves past a JSON number, or as far as the body follows its grammar.
func (s *keyScan) number() {
	digits := func() (n int) {
		for c := s.peek(); '0' <= c && c <= '9'; c = s.peek() {
			s.pos++
			n++
		}
		return n
	}
	if s.peek() == '-' {
		s.pos++
	}
	if s.peek() == '0' {
		s.pos++
	} else if digits() == 0 {
		return
	}
	if s.peek() == '.' {
		s.pos++
		if digits() == 0 {
			return
		}
	}
	if c := s.peek(); c == 'e' || c == 'E' {
		s.pos++
		if c := s.peek(); c == '+' || c == '-' {
			s.pos++
		}
		digits()
	}
}

// child reads a member's or an element's value, with its step on the path when it is an object or
// an array, which may hold a key.
func (s *keyScan) child(step keyStep, t reflect.Type, depth int, countRows bool) error {
	if s.skipSpace(); s.peek() != '{' && s.peek() != '[' {
		return s.value(nil, depth, false)
	}
	s.path = append(s.path, step)
	if err := s.value(t, depth, countRows); err != nil {
		return err
	}
	s.path = s.path[:len(s.path)-1]
	return nil
}

func (s *keyScan) object(t reflect.Type, depth int) error {
	if depth > maxJSONDepth {
		return errNotScanned
	}
	s.pos++
	var fields *jsonFields
	var elem reflect.Type // each member's type, when t types the members
	if t != nil && t.Kind() == reflect.Struct {
		fields = jsonFieldsOf(t)
	} else if t != nil && t.Kind() == reflect.Map {
		elem = t.Elem()
	}
	for len(s.sets) < depth {
		s.sets = append(s.sets, new(keySet))
	}
	set := s.sets[depth-1]
	set.reset()
	start, pathLen := s.pos, len(s.path)
	if s.skipSpace(); s.peek() == '}' {
		s.pos++
		return nil
	}
	for {
		s.skipSpace()
		keyAt := s.pos
		key, err := s.key()
		if err != nil {
			return err
		}
		if s.skipSpace(); s.peek() != ':' {
			return errNotScanned
		}
		s.pos++
		child, countRows := elem, false
		if f := s.lookup(fields, key); f != nil {
			for _, l := range set.landed {
				if l.field != f {
					continue
				}
				if bytes.Equal(l.key, key) {
					return s.earlier(set, start, keyAt, pathLen, s.refuse("has the key %q twice", key))
				}
				return s.earlier(set, start, keyAt, pathLen, s.refuse("has the key %q twice, written %q and %q", f.name, l.key, key))
			}
			set.landed = append(set.landed, landedKey{f, key})
			child, countRows = f.typ, depth == 1 && f == s.rowsField
		} else if set.add(key) {
			return s.refuse("has the key %q twice", key)
		}
		if err := s.child(keyStep{key: key, isKey: true}, child, depth, countRows); err != nil {
			return s.earlier(set, start, keyAt, pathLen, err)
		}
		s.skipSpace()
		switch s.peek() {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return s.earlier(set, start, s.pos, pathLen, nil)
		default:
			return errNotScanned
		}
	}
}

func (s *keyScan) array(t reflect.Type, depth int, countRows bool) error {
	if depth > maxJSONDepth {
		return errNotScanned
	}
	s.pos++
	var elem reflect.Type
	if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
		elem = t.Elem()
	}
	if s.skipSpace(); s.peek() == ']' {
		s.pos++
		return nil
	}
	for n := 0; ; n++ {
		if countRows {
			s.rows++
		}
		if err := s.child(keyStep{index: n}, elem, depth, false); err != nil {
			return err
		}
		s.skipSpace()
		switch s.peek() {
		case ',':
			s.pos++
		case ']':
			s.pos++
			return nil
		default:
			return errNotScanned
		}
	}
}

// earlier returns the refusal of a key that the object whose members start at start has twice,
// up to and with the key at keyAt, else err: a big keySet compares its keys only now, and the first
// refusal in the body is the one returned. pathLen is the path's length at the object.
func (s *keyScan) earlier(set *keySet, start, keyAt, pathLen int, err error) error {
	if !set.big || !set.repeated() {
		return err
	}
	r := keyScan{data: s.data, pos: start}
	seen := map[string]bool{}
	for {
		if r.skipSpace(); r.pos > keyAt {
			return err // two hashes were equal, not two keys
		}
		k, kerr := r.key()
		if kerr != nil {
			return err
		}
		if seen[string(k)] {
			s.path = s.path[:pathLen]
			return s.refuse("has the key %q twice", k)
		}
		seen[string(k)] = true
		if r.skipSpace(); r.peek() != ':' {
			return err
		}
		r.pos++
		if r.value(nil, 0, false) != nil {
			return err
		}
		if r.skipSpace(); r.peek() != ',' {
			return err
		}
		r.pos++
	}
}

// skipString moves past the string that starts at s.pos.
func (s *keyScan) skipString() error {
	s.pos++
	for n := 0; n < 16 && s.pos < len(s.data); n++ { // most keys are short: no search call
		switch s.data[s.pos] {
		case '"':
			s.pos++
			return nil
		case '\\':
			s.pos += 2
		default:
			s.pos++
		}
	}
	for s.pos < len(s.data) {
		i := bytes.IndexByte(s.data[s.pos:], '"')
		if i < 0 {
			break
		}
		end, slashes := s.pos+i, 0
		for j := end - 1; j >= 0 && s.data[j] == '\\'; j-- {
			slashes++
		}
		s.pos = end + 1
		if slashes%2 == 0 {
			return nil
		}
	}
	return errNotScanned
}

// key reads an object key as the decoder reads it: its bytes, or, where it has an escape or a byte
// past ASCII, the string it unquotes to (appendUnquotedJSON).
func (s *keyScan) key() ([]byte, error) {
	if s.peek() != '"' {
		return nil, errNotScanned
	}
	start := s.pos
	if err := s.skipString(); err != nil {
		return nil, err
	}
	raw := s.data[start+1 : s.pos-1]
	for i, c := range raw {
		if c == '\\' || c >= utf8.RuneSelf {
			at := len(s.arena)
			if n := 3 * len(raw); cap(s.arena)-at < n { // unquoted, a byte is at most three
				s.arena = slices.Grow(s.arena, max(at, n))
			}
			var ok bool
			if s.arena, ok = appendUnquotedJSON(append(s.arena, raw[:i]...), raw[i:]); !ok {
				return nil, errNotScanned
			}
			return s.arena[at:len(s.arena):len(s.arena)], nil
		}
	}
	return raw, nil
}

// appendUnquotedJSON appends the string whose quoted body is raw as encoding/json reads it (its
// unquote): escapes resolved, and a lone surrogate or a byte that is not UTF-8 read as U+FFFD. It
// reports false where raw is not a well-formed string body.
func appendUnquotedJSON(dst, raw []byte) ([]byte, bool) {
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case c == '\\' && i+1 < len(raw):
			esc := raw[i+1]
			i += 2
			switch esc {
			case '"', '\\', '/':
				dst = append(dst, esc)
			case 'b':
				dst = append(dst, '\b')
			case 'f':
				dst = append(dst, '\f')
			case 'n':
				dst = append(dst, '\n')
			case 'r':
				dst = append(dst, '\r')
			case 't':
				dst = append(dst, '\t')
			case 'u':
				r := hex4(raw[i:])
				if r < 0 {
					return dst, false
				}
				i += 4
				if utf16.IsSurrogate(r) {
					if i+1 < len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
						if d := utf16.DecodeRune(r, hex4(raw[i+2:])); d != unicode.ReplacementChar {
							r, i = d, i+6
						} else {
							r = unicode.ReplacementChar
						}
					} else {
						r = unicode.ReplacementChar
					}
				}
				dst = utf8.AppendRune(dst, r)
			default:
				return dst, false
			}
		case c == '\\' || c == '"' || c < ' ':
			return dst, false
		case c < utf8.RuneSelf:
			j := i + 1
			for j < len(raw) && raw[j] < utf8.RuneSelf && raw[j] >= ' ' && raw[j] != '\\' && raw[j] != '"' {
				j++
			}
			dst, i = append(dst, raw[i:j]...), j
		case c < 0xc2 || c > 0xf4: // never starts a rune
			dst, i = append(dst, "\uFFFD"...), i+1
		default:
			r, n := utf8.DecodeRune(raw[i:])
			dst = utf8.AppendRune(dst, r)
			i += n
		}
	}
	return dst, true
}

// hex4 reads four hex digits, or returns -1.
func hex4(b []byte) rune {
	if len(b) < 4 {
		return -1
	}
	var r rune
	for _, c := range b[:4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		case 'A' <= c && c <= 'F':
			c -= 'A' - 10
		default:
			return -1
		}
		r = r*16 + rune(c)
	}
	return r
}

// lookup is fields.lookup for a key's bytes, folding into the scan's scratch buffer.
func (s *keyScan) lookup(fields *jsonFields, key []byte) *jsonField {
	if fields == nil {
		return nil
	}
	if f, ok := fields.exact[string(key)]; ok {
		return f
	}
	s.fold = appendFoldedJSONName(s.fold[:0], key)
	return fields.folded[string(s.fold)]
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// JSONFieldName returns the JSON name of the field of dst's struct that encoding/json decodes the
// object key into, which differs from key when it matched only ignoring case, or key when no field
// takes it.
func JSONFieldName(dst any, key string) string {
	if t := derefType(reflect.TypeOf(dst)); t != nil && t.Kind() == reflect.Struct {
		if f := jsonFieldsOf(t).lookup(key); f != nil {
			return f.name
		}
	}
	return key
}

// jsonField is a struct field encoding/json decodes an object key into.
type jsonField struct {
	name string // the json tag's name, else the Go field name
	typ  reflect.Type
}

// jsonFields is a struct type's fields by name, and by name folded as encoding/json folds it, where
// the first field in declaration order takes a folded name, as it does there.
type jsonFields struct {
	exact, folded map[string]*jsonField
}

// lookup returns the field encoding/json decodes key into, or nil when no field takes it.
func (fs *jsonFields) lookup(key string) *jsonField {
	if fs == nil {
		return nil
	}
	if f, ok := fs.exact[key]; ok {
		return f
	}
	return fs.folded[foldJSONName(key)]
}

var jsonFieldsCache sync.Map // reflect.Type -> *jsonFields

// jsonFieldsOf lists the fields of struct type t as encoding/json's typeFields does: exported
// fields, and the fields of an embedded struct whether or not it is exported, named by the json tag
// or else the Go name, with `-` left out. An untagged embedded struct's fields are promoted; where a
// name repeats, the shallowest field wins, a tagged one breaks a tie, and a tie that stays (or the
// same struct embedded twice at one depth) leaves no field of that name.
func jsonFieldsOf(t reflect.Type) *jsonFields {
	if fs, ok := jsonFieldsCache.Load(t); ok {
		return fs.(*jsonFields)
	}
	type embedded struct {
		typ   reflect.Type
		index []int
	}
	var all []jsonFieldCandidate
	next, nextCount := []embedded{{typ: t}}, map[reflect.Type]int{t: 1}
	visited := map[reflect.Type]bool{}
	for len(next) > 0 {
		current, count := next, nextCount
		next, nextCount = nil, map[reflect.Type]int{}
		for _, e := range current {
			if visited[e.typ] {
				continue
			}
			visited[e.typ] = true
			for i := range e.typ.NumField() {
				sf := e.typ.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if !validJSONTagName(name) {
					name = ""
				}
				index := append(slices.Clone(e.index), i)
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					c := jsonFieldCandidate{jsonField{cmp.Or(name, sf.Name), sf.Type}, index, name != ""}
					all = append(all, c)
					if count[e.typ] > 1 { // the same struct embedded twice: a second copy annuls the name
						all = append(all, c)
					}
					continue
				}
				if nextCount[ft]++; nextCount[ft] == 1 {
					next = append(next, embedded{ft, index})
				}
			}
		}
	}
	kept := dominantJSONFields(all)
	fs := &jsonFields{exact: map[string]*jsonField{}, folded: map[string]*jsonField{}}
	for i := range kept {
		f := &kept[i].jsonField
		fs.exact[f.name] = f
		if folded := foldJSONName(f.name); fs.folded[folded] == nil {
			fs.folded[folded] = f
		}
	}
	got, _ := jsonFieldsCache.LoadOrStore(t, fs)
	return got.(*jsonFields)
}

// jsonFieldCandidate is a field jsonFieldsOf finds, before the fields of one name are narrowed to one.
type jsonFieldCandidate struct {
	jsonField
	index  []int
	tagged bool
}

// dominantJSONFields keeps, of the candidates that share a name, the one encoding/json decodes that
// name into: the shallowest, and of those a tagged one; where two are still level, no field takes it.
// The kept fields come back in declaration order.
func dominantJSONFields(all []jsonFieldCandidate) []jsonFieldCandidate {
	slices.SortStableFunc(all, func(a, b jsonFieldCandidate) int {
		return cmp.Or(strings.Compare(a.name, b.name), cmp.Compare(len(a.index), len(b.index)),
			cmp.Compare(boolRank(b.tagged), boolRank(a.tagged)), slices.Compare(a.index, b.index))
	})
	var kept []jsonFieldCandidate
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && all[j].name == all[i].name {
			j++
		}
		if j-i == 1 || len(all[i].index) != len(all[i+1].index) || all[i].tagged != all[i+1].tagged {
			kept = append(kept, all[i])
		}
		i = j
	}
	slices.SortFunc(kept, func(a, b jsonFieldCandidate) int { return slices.Compare(a.index, b.index) })
	return kept
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// validJSONTagName is encoding/json's isValidTag: a tag name with any other character is ignored
// and the Go field name is used.
func validJSONTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// foldJSONName folds a name as encoding/json does to match a key to a field (its foldName): an ASCII
// letter to upper case, any other rune to the smallest rune of its Unicode simple-fold orbit, so "ſ"
// (U+017F) folds with "s" and the Kelvin sign with "k".
func foldJSONName(name string) string {
	return string(appendFoldedJSONName(nil, []byte(name)))
}

// appendFoldedJSONName appends name folded (foldJSONName) to dst.
func appendFoldedJSONName(dst, name []byte) []byte {
	for i := 0; i < len(name); {
		r, n := rune(name[i]), 1
		if r < utf8.RuneSelf {
			if 'a' <= r && r <= 'z' {
				r -= 'a' - 'A'
			}
			dst = append(dst, byte(r))
			i++
			continue
		}
		r, n = utf8.DecodeRune(name[i:])
		for {
			f := unicode.SimpleFold(r)
			if f <= r {
				r = f
				break
			}
			r = f
		}
		dst = utf8.AppendRune(dst, r)
		i += n
	}
	return dst
}
