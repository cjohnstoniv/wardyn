// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"fmt"
	"strings"
)

// maxTipNodes bounds the tree entries Tips reaches for one push, every
// command's walk together, as maxTreeNodes bounds Inspect's walk. A var so a
// test can shrink it.
var maxTipNodes = maxTreeNodes

// Tips says, for each of r.Commands in order, what the tree its new value sets
// holds at the paths want picks: the tree itself, not what each commit
// introduces. want is asked of every entry the walk reaches, as a Change with
// its path, mode and object id; for a directory it answers whether anything at
// or beneath that path matters. A file or link it picks is one entry, and so
// is a directory it picks that the pack does not carry (ModeUncarried: the
// receiving side holds its contents); a directory the pack carries is walked
// into. A root tree the pack does not carry is one entry at "". A delete, and
// every command of a push that carries no object (Empty), gets none.
//
// The walks together are held to maxTipNodes entries reached, maxChanges
// entries kept, maxPathBytes of path text and maxTreeDepth levels. Past any of
// them the answer is an ErrTooLarge, never a short list.
func (r Result) Tips(want func(Change) (bool, error)) ([][]Change, error) {
	out := make([][]Change, len(r.Commands))
	if r.idx == nil {
		return out, nil
	}
	nodes, kept, pathBytes := 0, 0, 0
	keep := func(i int, c Change) error {
		if kept++; kept > maxChanges {
			return fmt.Errorf("%w: the push's branches hold more than %d matched paths", ErrTooLarge, maxChanges)
		}
		out[i] = append(out[i], c)
		return nil
	}
	var walk func(i int, prefix, oid string, depth int) error
	walk = func(i int, prefix, oid string, depth int) error {
		if depth > maxTreeDepth {
			return ErrTooDeep
		}
		entries, carried, err := r.idx.tree(oid, maxTipNodes-nodes)
		if err != nil {
			return err
		}
		if !carried {
			return keep(i, Change{Path: strings.TrimSuffix(prefix, "/"), Mode: ModeUncarried, size: -1, OID: oid})
		}
		// tree refused a listing longer than what was left, so this stays in bounds.
		nodes += len(entries)
		for _, e := range entries {
			c := Change{Path: prefix + e.name, Mode: e.mode, size: r.idx.blobSize(e.oid), OID: e.oid}
			if pathBytes += len(c.Path); pathBytes > maxPathBytes {
				return fmt.Errorf("%w: the push's paths come to more than %d bytes", ErrTooLarge, maxPathBytes)
			}
			picked, err := want(c)
			switch {
			case err != nil:
				return err
			case !picked:
			case e.isTree():
				err = walk(i, c.Path+"/", e.oid, depth+1)
			default:
				err = keep(i, c)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	for i, cmd := range r.Commands {
		if isZeroOID(cmd.New) {
			continue
		}
		tip, how := r.idx.peel(cmd.New)
		if how != peeled {
			continue
		}
		// Inspect refuses a pack holding a commit that does not parse.
		c, err := parseCommit(r.idx.byOID[tip].data, r.idx.format.size)
		if err != nil {
			return nil, err
		}
		if err := walk(i, "", c.tree, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Empty reports whether the push carries no object at all. Every ref it sets
// then names a commit the receiving side already holds, and Tips gives it
// nothing: what such a commit holds is read where it is held.
func (r Result) Empty() bool { return r.idx == nil }
