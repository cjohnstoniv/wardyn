// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package assistant holds the pure parts of the assistant: how people's names become letters, how a turn
// and a digest are built, the instructions the model is given and the records its first message carries.
// It reads nothing and writes nothing, so it is tested without a server.
package assistant

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Person is one human the records name. Either field may be empty; Email is the stronger identity.
type Person struct {
	Email string
	Name  string
}

type lettered struct {
	label string
	email string
	name  string
}

// Letters swaps people's e-mail addresses and display names for Person A, Person B and so on. Letters
// are handed out in the order people are added, one per person, and stay stable for the session. The
// browser restores names from the map the server returns; Restore exists for tests.
type Letters struct {
	people []lettered
	re     *regexp.Regexp
	byKey  map[string]string
}

// NewLetters is an empty set. Add people to it before applying it.
func NewLetters() *Letters { return &Letters{byKey: map[string]string{}} }

// LettersFromMap rebuilds a set from the map a session stored, label to the name it stands for. The labels
// keep their letters.
func LettersFromMap(m map[string]string) *Letters {
	l := NewLetters()
	for _, label := range slices.Sorted(maps.Keys(m)) {
		l.people = append(l.people, lettered{label: label, email: m[label]})
		l.byKey[strings.ToLower(m[label])] = label
	}
	l.re = nil
	return l
}

// Add gives p a letter, or returns the one they already have. A person with neither an address nor a name
// gets none and Add returns "".
func (l *Letters) Add(p Person) string {
	email, name := strings.TrimSpace(p.Email), strings.TrimSpace(p.Name)
	for _, k := range []string{email, name} {
		if k == "" {
			continue
		}
		if label, ok := l.byKey[strings.ToLower(k)]; ok {
			l.learn(label, email, name)
			return label
		}
	}
	if email == "" && name == "" {
		return ""
	}
	label := "Person " + letterName(len(l.people))
	l.people = append(l.people, lettered{label: label, email: email, name: name})
	for _, k := range []string{email, name} {
		if k != "" {
			l.byKey[strings.ToLower(k)] = label
		}
	}
	l.re = nil
	return label
}

// learn adds the e-mail or name a repeat sighting brings to an existing person.
func (l *Letters) learn(label, email, name string) {
	i := slices.IndexFunc(l.people, func(p lettered) bool { return p.label == label })
	if i < 0 {
		return
	}
	if l.people[i].email == "" && email != "" {
		l.people[i].email = email
		l.byKey[strings.ToLower(email)] = label
		l.re = nil
	}
	if l.people[i].name == "" && name != "" {
		l.people[i].name = name
		l.byKey[strings.ToLower(name)] = label
		l.re = nil
	}
}

// letterName is A..Z, then AA, AB and so on.
func letterName(i int) string {
	s := ""
	for {
		s = string(rune('A'+i%26)) + s
		i = i/26 - 1
		if i < 0 {
			return s
		}
	}
}

// Map is each label with the name it stands for: the address when there is one, else the display name.
func (l *Letters) Map() map[string]string {
	m := make(map[string]string, len(l.people))
	for _, p := range l.people {
		m[p.label] = cmpOr(p.email, p.name)
	}
	return m
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// compile builds one matcher over every address and name, longest first, so a whole address wins over a
// name inside it.
func (l *Letters) compile() {
	var keys []string
	for _, p := range l.people {
		for _, k := range []string{p.email, p.name} {
			if k != "" {
				keys = append(keys, k)
			}
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	if len(keys) == 0 {
		l.re = nil
		return
	}
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = regexp.QuoteMeta(k)
	}
	l.re = regexp.MustCompile("(?i)" + strings.Join(quoted, "|"))
}

// Apply replaces each whole address and each name in text with its letter. A match must sit on token
// boundaries, so a bare local part such as api never reaches into a host name and a name inside a longer
// word is left alone.
func (l *Letters) Apply(text string) string {
	if l.re == nil {
		l.compile()
	}
	if l.re == nil {
		return text
	}
	var b strings.Builder
	last, from := 0, 0
	for from <= len(text) {
		loc := l.re.FindStringIndex(text[from:])
		if loc == nil {
			break
		}
		start, end := from+loc[0], from+loc[1]
		if end == start || !boundaryBefore(text, start) || !boundaryAfter(text, end) {
			from = start + 1
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(l.byKey[strings.ToLower(text[start:end])])
		last, from = end, end
	}
	b.WriteString(text[last:])
	return b.String()
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '@'
}

func boundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !isTokenRune(r) && r != '.' && r != '+' && r != '%'
}

func boundaryAfter(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	r, size := utf8.DecodeRuneInString(text[i:])
	if isTokenRune(r) {
		return false
	}
	if r == '.' && i+size < len(text) {
		next, _ := utf8.DecodeRuneInString(text[i+size:])
		return !isTokenRune(next)
	}
	return true
}

// addressRE matches an e-mail address.
var addressRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

// PeopleNamedIn is everyone a record's JSON names, in the order they appear: the subject of every object that
// assigns something to a person ({"subject_type": "user", "subject": ...}), and every e-mail address in any
// string. A subject that is not an address is a name. Keys are walked in sorted order, so the order is stable.
func PeopleNamedIn(raw []byte) []Person {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []Person
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if st, _ := t["subject_type"].(string); st == "user" {
				if sub, _ := t["subject"].(string); strings.TrimSpace(sub) != "" {
					out = append(out, PersonOf(sub))
				}
			}
			for _, k := range slices.Sorted(maps.Keys(t)) {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case string:
			for _, a := range addressRE.FindAllString(t, -1) {
				out = append(out, Person{Email: a})
			}
		}
	}
	walk(v)
	return out
}

// PersonOf is a principal as a person: its e-mail when it is one, else its name.
func PersonOf(subject string) Person {
	if strings.Contains(subject, "@") {
		return Person{Email: subject}
	}
	return Person{Name: subject}
}

// Restore puts each letter in text back to the name it stands for. Only tests use it.
func (l *Letters) Restore(text string) string {
	pairs := make([]string, 0, 2*len(l.people))
	for _, p := range slices.Backward(slices.Clone(l.people)) {
		pairs = append(pairs, p.label, cmpOr(p.email, p.name))
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// String is for test failures.
func (l *Letters) String() string { return fmt.Sprint(l.Map()) }
