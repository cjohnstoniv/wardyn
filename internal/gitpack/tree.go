// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// modeTree is the mode a tree entry carries for a subdirectory. Note the missing
// leading zero: `git ls-tree` prints "040000", the object itself stores "40000".
const modeTree = "40000"

// treeEntry is one entry of a tree object: a mode, a name and the object id of
// what the name points at.
type treeEntry struct{ mode, name, oid string }

func (e treeEntry) isTree() bool { return e.mode == modeTree }

// parseTree reads a tree object: "<mode> <name>\0<object-id>" repeated, with the
// id raw rather than hex and the whole thing unterminated.
//
// The name checks are not decoration. A path is what a deny rule matches, so a
// name carrying a slash or a "." segment would let a crafted tree describe a
// path other than the one it occupies. git refuses to write such a tree; this
// refuses to read one.
func parseTree(data []byte, hashLen int) ([]treeEntry, error) {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("gitpack: tree entry has no mode")
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 {
			return nil, fmt.Errorf("gitpack: tree entry %q has no name terminator", mode)
		}
		name := string(rest[:nul])
		rest = rest[nul+1:]
		if len(rest) < hashLen {
			return nil, fmt.Errorf("gitpack: tree entry %q is truncated at its object id", name)
		}
		if err := checkEntry(mode, name); err != nil {
			return nil, err
		}
		out = append(out, treeEntry{mode: mode, name: name, oid: hex.EncodeToString(rest[:hashLen])})
		data = rest[hashLen:]
	}
	return out, nil
}

func checkEntry(mode, name string) error {
	if _, err := strconv.ParseUint(mode, 8, 32); err != nil || mode == "" || len(mode) > 6 {
		return fmt.Errorf("gitpack: tree entry has a mode of %q", mode)
	}
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("gitpack: tree entry names a path component of %q", name)
	}
	return nil
}

// tree returns the entries of the tree oid names. ok is false when the pack does
// not carry that tree, which means the receiving side already stores it.
func (i *index) tree(oid string) (entries []treeEntry, ok bool, err error) {
	o, held := i.byOID[oid]
	if !held {
		return nil, false, nil
	}
	if o.typ != objTree {
		return nil, false, fmt.Errorf("gitpack: %s is a %s where a tree was expected", oid, o.typ.label())
	}
	entries, err = parseTree(o.data, i.format.size)
	return entries, err == nil, err
}

// commitInfo is the part of a commit object this package reads.
type commitInfo struct {
	tree    string
	parents []string
}

// parseCommit reads the headers of a commit object, which run to the first blank
// line.
func parseCommit(data []byte, hashLen int) (commitInfo, error) {
	var c commitInfo
	for _, line := range strings.Split(headersOf(data), "\n") {
		key, val, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		switch key {
		case "tree":
			c.tree = val
		case "parent":
			c.parents = append(c.parents, val)
		default:
			continue
		}
		if len(val) != hashLen*2 || !isHex(val) {
			return commitInfo{}, fmt.Errorf("gitpack: commit header %q is not an object id of this format", line)
		}
	}
	if c.tree == "" {
		return commitInfo{}, fmt.Errorf("gitpack: commit object has no tree header")
	}
	return c, nil
}

// tagTarget is the object id an annotated tag points at.
func tagTarget(data []byte, hashLen int) (string, bool) {
	for _, line := range strings.Split(headersOf(data), "\n") {
		if val, ok := strings.CutPrefix(line, "object "); ok {
			return val, len(val) == hashLen*2 && isHex(val)
		}
	}
	return "", false
}

func headersOf(data []byte) string {
	head, _, _ := strings.Cut(string(data), "\n\n")
	return head
}

// peel follows tag objects down to the commit they name, and reports whether
// that commit is in the pack.
func (i *index) peel(oid string) (string, bool) {
	for range maxPeel {
		o, ok := i.byOID[oid]
		if !ok {
			return "", false
		}
		if o.typ == objCommit {
			return oid, true
		}
		if o.typ != objTag {
			return "", false
		}
		if oid, ok = tagTarget(o.data, i.format.size); !ok {
			return "", false
		}
	}
	return "", false
}

// coverCommands refuses a request whose ref updates the pack does not account
// for. A push that creates a ref at an object the receiving side already holds
// sends an empty pack: there is nothing wrong with it, and nothing was read
// about what that ref now points at either.
func (i *index) coverCommands(cmds []Command) error {
	for _, c := range cmds {
		if isZeroOID(c.New) {
			continue
		}
		if _, ok := i.peel(c.New); !ok {
			return fmt.Errorf("%w: %s is set to %s, which the pack does not carry as a commit",
				ErrUninspectable, c.Ref, c.New)
		}
	}
	return nil
}

// changes is the union of what every commit in the pack introduces.
//
// Every commit, not only the ones the commands name: a push of three commits
// carries all three, and diffing only the tip against its parent would miss what
// the two below it introduced — a file added in the first commit and left alone
// afterwards would go unreported. The union is walked in object-id order so that
// the same pack always produces the same answer.
func (i *index) changes() ([]Change, error) {
	w := &walker{idx: i, seen: map[Change]bool{}}
	for _, oid := range slices.Sorted(maps.Keys(i.byOID)) {
		if i.byOID[oid].typ != objCommit {
			continue
		}
		c, err := parseCommit(i.byOID[oid].data, i.format.size)
		if err != nil {
			return nil, err
		}
		if err := w.introduced(c); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(w.out, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.Mode, b.Mode), cmp.Compare(a.Size, b.Size))
	})
	return w.out, nil
}

// walker accumulates one pack's change set, deduplicated on the whole entry:
// the same path at two modes is two answers, because a rule about executables
// must see the executable one.
type walker struct {
	idx  *index
	seen map[Change]bool
	out  []Change
}

// introduced reports what one commit brings in: a diff against every parent the
// pack carries, or — when it carries none — the commit's whole tree. See the
// package comment for why the second case over-reports and why that is the
// direction to err in.
func (w *walker) introduced(c commitInfo) error {
	diffed := false
	for _, p := range c.parents {
		o, ok := w.idx.byOID[p]
		if !ok || o.typ != objCommit {
			continue
		}
		parent, err := parseCommit(o.data, w.idx.format.size)
		if err != nil {
			return err
		}
		if err := w.diff("", parent.tree, c.tree, 0); err != nil {
			return err
		}
		diffed = true
	}
	if diffed {
		return nil
	}
	return w.walk("", c.tree, 0)
}

// diff reports the entries the new tree adds or changes. Removals are not
// reported: the enumerated case has no pre-image to notice them in, and one
// answer that is sometimes richer than the other is worse than one that always
// means the same thing.
func (w *walker) diff(prefix, oldOID, newOID string, depth int) error {
	if oldOID == newOID {
		return nil
	}
	if depth > maxTreeDepth {
		return fmt.Errorf("gitpack: trees nested deeper than %d", maxTreeDepth)
	}
	next, ok, err := w.idx.tree(newOID)
	if err != nil || !ok {
		return err
	}
	prev, ok, err := w.idx.tree(oldOID)
	if err != nil {
		return err
	}
	if !ok {
		return w.walkEntries(prefix, next, depth)
	}
	was := make(map[string]treeEntry, len(prev))
	for _, e := range prev {
		was[e.name] = e
	}
	for _, e := range next {
		before, existed := was[e.name]
		if existed && before.mode == e.mode && before.oid == e.oid {
			continue
		}
		path := prefix + e.name
		switch {
		case !e.isTree():
			err = w.leaf(path, e)
		case existed && before.isTree():
			err = w.diff(path+"/", before.oid, e.oid, depth+1)
		default:
			err = w.walk(path+"/", e.oid, depth+1)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// walk reports every entry under one tree. A subtree the pack does not carry is
// skipped, not refused: see the package comment.
func (w *walker) walk(prefix, oid string, depth int) error {
	entries, ok, err := w.idx.tree(oid)
	if err != nil || !ok {
		return err
	}
	return w.walkEntries(prefix, entries, depth)
}

func (w *walker) walkEntries(prefix string, entries []treeEntry, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("gitpack: trees nested deeper than %d", maxTreeDepth)
	}
	for _, e := range entries {
		path := prefix + e.name
		var err error
		if e.isTree() {
			err = w.walk(path+"/", e.oid, depth+1)
		} else {
			err = w.leaf(path, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) leaf(path string, e treeEntry) error {
	c := Change{Path: path, Mode: e.mode, Size: w.idx.blobSize(e.oid)}
	if w.seen[c] {
		return nil
	}
	if len(w.out) >= maxChanges {
		return fmt.Errorf("%w: the push touches more than %d paths", ErrUninspectable, maxChanges)
	}
	w.seen[c] = true
	w.out = append(w.out, c)
	return nil
}
