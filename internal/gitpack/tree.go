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

// modeTree and modeFormat are git's S_ISDIR: a tree entry is a directory when
// the format bits of its mode are S_IFDIR, whatever the remaining bits say.
//
// `git ls-tree` prints "040000" and a tree object normally stores "40000", but
// git parses the digits and MASKS them, so "040000", "40001", "40644" and
// "47777" are all directories it recurses into. Neither matching the string
// "40000" nor comparing the whole number for equality sees that: the entry
// became a leaf, and every path beneath it vanished from the answer. Nothing
// downstream covers the gap either — receive.fsckObjects rejects "040000" as
// zeroPaddedFilemode but lets "40001" through with a badFilemode warning.
const (
	modeTree    = 0o040000
	modeRegular = 0o100000
	modeFormat  = 0o170000
)

// ModeUncarried is the Mode of a Change that stands for a directory whose tree
// object the pack does not hold. No other Change carries a directory mode: a
// directory the pack does carry is reported as the entries beneath it.
const ModeUncarried = "40000"

// Opaque reports whether a checkout may hold paths beneath c.Path that no
// Change names: a directory whose tree the pack does not carry, a symlink, or a
// submodule. Anything that is not a regular file counts — git checks out every
// mode it cannot classify as a submodule pointer — and so does a mode that does
// not parse, because the safe answer to "what is under this?" is "anything".
//
// A rule about a path beneath an opaque entry cannot be decided from the pack,
// so a deny rule must treat one as matched rather than as absent.
func (c Change) Opaque() bool {
	m, err := strconv.ParseUint(c.Mode, 8, 32)
	return err != nil || m&modeFormat != modeRegular
}

// treeEntry is one entry of a tree object: a mode, a name and the object id of
// what the name points at.
type treeEntry struct{ mode, name, oid string }

// isTree masks the parsed mode, never the spelling. parseTree has already
// rejected a mode that is not octal.
func (e treeEntry) isTree() bool {
	m, err := strconv.ParseUint(e.mode, 8, 32)
	return err == nil && m&modeFormat == modeTree
}

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
//
// Git's own parser takes the FIRST tree header and stops its parent loop at the
// first header that is not a parent, so a second "tree" line — or a "parent"
// line after the author — is a header git never sees while a last-wins reader
// takes it as authoritative. A commit shaped that way is hostile by
// construction and no git writes one, so it is refused rather than reconciled:
// the two sides disagreeing about what a push contains is the one answer this
// package must never give.
func parseCommit(data []byte, hashLen int) (commitInfo, error) {
	var c commitInfo
	inParentBlock := true
	for _, line := range strings.Split(headersOf(data), "\n") {
		key, val, found := strings.Cut(line, " ")
		switch {
		case !found:
			inParentBlock = false
			continue
		case key == "tree":
			if c.tree != "" {
				return commitInfo{}, fmt.Errorf("%w: the commit object carries more than one tree header",
					ErrUninspectable)
			}
			c.tree = val
		case key == "parent":
			if !inParentBlock {
				return commitInfo{}, fmt.Errorf("%w: the commit object carries a parent header git would not read",
					ErrUninspectable)
			}
			c.parents = append(c.parents, val)
		default:
			inParentBlock = false
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

// changes is the union of what every commit in the pack introduces, and the
// commits it builds on. A commit held marks true is one the receiving side
// already has (Settle): it introduces nothing, and is a base.
//
// Every commit, not only the ones the commands name: a push of three commits
// carries all three, and diffing only the tip against its parent would miss what
// the two below it introduced — a file added in the first commit and left alone
// afterwards would go unreported. The union is walked in object-id order so that
// the same pack always produces the same answer.
func (i *index) changes(held map[string]bool) ([]Change, []string, error) {
	w := newWalker(i)
	w.held = held
	for _, oid := range slices.Sorted(maps.Keys(i.byOID)) {
		if i.byOID[oid].typ != objCommit || held[oid] {
			continue
		}
		c, err := parseCommit(i.byOID[oid].data, i.format.size)
		if err != nil {
			return nil, nil, err
		}
		if err := w.introduced(c); err != nil {
			return nil, nil, err
		}
	}
	slices.SortFunc(w.out, func(a, b Change) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.Mode, b.Mode),
			cmp.Compare(a.Size, b.Size), strings.Compare(a.OID, b.OID))
	})
	return w.out, slices.Sorted(maps.Keys(w.bases)), nil
}

// Settle is the same answer with the history the receiving side already holds
// taken out. held reports whether one commit the pack carries is already in
// history the caller trusts. A commit it holds introduces nothing and neither
// does anything beneath it, since its parents are in that history too; a
// commit built on one is diffed against it from the pack's own trees, and it
// joins Bases. Object ids are content addresses, so the copy the pack carries
// is the one the receiving side holds.
//
// held is asked as little as possible. A commit nothing in the pack builds on
// is what the push is for and is never asked about. A commit with no parent in
// the pack is asked first: when it is not held, nothing above it is either, so
// a push of only new commits costs one question. Then the pack is walked down
// from its tips, and a held commit answers for everything beneath it. A commit
// once found new is never taken as held. An error from held is returned with an
// empty Result.
func (r Result) Settle(held func(commit string) (bool, error)) (Result, error) {
	if r.idx == nil {
		return r, nil
	}
	var commits []string
	down, up := map[string][]string{}, map[string][]string{} // carried parents, carried children
	for _, oid := range slices.Sorted(maps.Keys(r.idx.byOID)) {
		if r.idx.byOID[oid].typ != objCommit {
			continue
		}
		c, err := parseCommit(r.idx.byOID[oid].data, r.idx.format.size)
		if err != nil {
			return Result{}, err
		}
		commits = append(commits, oid)
		for _, p := range c.parents {
			if o, ok := r.idx.byOID[p]; ok && o.typ == objCommit {
				down[oid] = append(down[oid], p)
				up[p] = append(up[p], oid)
			}
		}
	}
	isHeld := map[string]bool{} // an entry is a commit classified: held, or new
	spread := func(from string, v bool, next map[string][]string) {
		for stack := []string{from}; len(stack) > 0; {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if was, ok := isHeld[c]; ok && (!was || v) {
				continue
			}
			isHeld[c] = v
			stack = append(stack, next[c]...)
		}
	}
	ask := func(c string) (bool, error) {
		if v, ok := isHeld[c]; ok {
			return v, nil
		}
		v, err := held(c)
		if err != nil {
			return false, err
		}
		if v {
			spread(c, true, down)
		} else {
			spread(c, false, up)
		}
		return isHeld[c], nil
	}
	var tips []string
	for _, c := range commits {
		if len(up[c]) == 0 {
			tips = append(tips, c)
			isHeld[c] = false
		}
	}
	for _, c := range commits {
		if len(down[c]) == 0 {
			if _, err := ask(c); err != nil {
				return Result{}, err
			}
		}
	}
	walked := map[string]bool{}
	for queue := tips; len(queue) > 0; queue = queue[1:] {
		if walked[queue[0]] {
			continue
		}
		walked[queue[0]] = true
		for _, p := range down[queue[0]] {
			v, err := ask(p)
			if err != nil {
				return Result{}, err
			}
			if !v {
				queue = append(queue, p)
			}
		}
	}
	out := r
	var err error
	if out.Changes, out.Bases, err = r.idx.changes(isHeld); err != nil {
		return Result{}, err
	}
	return out, nil
}

// walker accumulates one pack's change set, deduplicated on the whole entry:
// the same path at two modes is two answers, because a rule about executables
// must see the executable one.
type walker struct {
	idx  *index
	seen map[Change]bool
	// walked is every (prefix, tree) pair already expanded, for the duration of
	// one inspection. Trees are a DAG: a pack of 64 levels that each name the
	// level below twice is 3 KB of objects and 2^65 expansions, and maxTreeDepth
	// bounds the depth of that walk, not its width. The PREFIX belongs in the key
	// because the same subtree reached by two paths contributes leaves under
	// both, and dropping it would under-report. With it the skip is exact:
	// expanding one tree under one prefix is a pure function of that pair — the
	// depth is the prefix's component count — and leaf already deduplicates, so
	// the second expansion could only re-emit what the first did.
	walked map[string]bool
	// diffed is every (prefix, old tree, new tree) comparison already made, a
	// skip exact for the same reason walked's is. A merge compared against one
	// parent re-compares every directory the other side changed — the same
	// comparisons that side's own commits made — and charging them again cost
	// merge-heavy history up to six times what charging each once does (#254).
	diffed map[string]bool
	// nodes counts the tree ENTRIES walked, against maxTreeNodes. The memo
	// collapses a repeat of the same path; it cannot collapse b^d distinct paths
	// through d levels of fan-out, and a fan-out whose subtrees resolve to no
	// leaves never reaches maxChanges either. Entries rather than one unit per
	// expansion, because every entry costs a lookup whether or not the pack
	// carries what it names — charging per expansion left width free, and a
	// wide-and-deep pack spent minutes inside the ceiling.
	nodes int
	out   []Change
	// bases are the parents the pack's commits name and the pack does not
	// carry, or that held marks (Result.Bases).
	bases map[string]bool
	held  map[string]bool
}

func newWalker(i *index) *walker {
	return &walker{idx: i, seen: map[Change]bool{}, walked: map[string]bool{}, diffed: map[string]bool{},
		bases: map[string]bool{}}
}

// charge accounts for n tree entries about to be walked.
func (w *walker) charge(n int) error {
	if w.nodes += n; w.nodes > maxTreeNodes {
		return fmt.Errorf("%w: the push walks more than %d tree entries", ErrUninspectable, maxTreeNodes)
	}
	return nil
}

// introduced reports what one commit brings in: a diff against every parent the
// pack carries, or — when it carries none — the commit's whole tree. See the
// package comment for why the second case over-reports and why that is the
// direction to err in. A parent the pack does not carry is recorded as a base,
// and so is a held one, which is also diffed against.
func (w *walker) introduced(c commitInfo) error {
	diffed := false
	for _, p := range c.parents {
		o, ok := w.idx.byOID[p]
		if !ok || w.held[p] {
			w.bases[p] = true
		}
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
//
// An entry whose object id matches the pre-image's at the same name is skipped,
// and that skip is exact: this is the one place the pack proves a directory it
// does not carry is the one that stood there before.
func (w *walker) diff(prefix, oldOID, newOID string, depth int) error {
	if oldOID == newOID {
		return nil
	}
	key := prefix + "\x00" + oldOID + "\x00" + newOID
	if w.diffed[key] {
		return nil
	}
	w.diffed[key] = true
	if depth > maxTreeDepth {
		return fmt.Errorf("gitpack: trees nested deeper than %d", maxTreeDepth)
	}
	next, ok, err := w.idx.tree(newOID)
	if err != nil {
		return err
	}
	if !ok {
		return w.uncarried(prefix, newOID)
	}
	prev, ok, err := w.idx.tree(oldOID)
	if err != nil {
		return err
	}
	if !ok {
		return w.walkEntries(prefix, next, depth)
	}
	if err := w.charge(len(prev) + len(next)); err != nil {
		return err
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
// reported as one opaque entry at its own path: see the package comment.
func (w *walker) walk(prefix, oid string, depth int) error {
	key := prefix + "\x00" + oid
	if w.walked[key] {
		return nil
	}
	entries, ok, err := w.idx.tree(oid)
	if err != nil {
		return err
	}
	if !ok {
		return w.uncarried(prefix, oid)
	}
	w.walked[key] = true
	return w.walkEntries(prefix, entries, depth)
}

func (w *walker) walkEntries(prefix string, entries []treeEntry, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("gitpack: trees nested deeper than %d", maxTreeDepth)
	}
	if err := w.charge(len(entries)); err != nil {
		return err
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
	return w.record(Change{Path: path, Mode: e.mode, Size: w.idx.blobSize(e.oid), OID: e.oid})
}

// uncarried reports a directory whose tree object the pack does not hold. Its
// contents are a tree the receiving side already stores, but nothing in the
// pack says it stood at THIS path before: a directory moved or copied onto a
// new path, or a commit whose whole root is an older tree, looks exactly like
// one left untouched. So it is reported, at its own path — the root is "" —
// as an opaque entry carrying its tree's id, never skipped.
func (w *walker) uncarried(prefix, oid string) error {
	return w.record(Change{Path: strings.TrimSuffix(prefix, "/"), Mode: ModeUncarried, Size: -1, OID: oid})
}

func (w *walker) record(c Change) error {
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
