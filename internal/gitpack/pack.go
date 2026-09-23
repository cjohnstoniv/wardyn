// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package gitpack answers one question about a buffered git-receive-pack
// request: which paths would this push change, with what file modes and sizes?
//
// It is a pure library. It opens no sockets, reads no configuration and talks to
// no part of the control plane: it never fetches the base objects a thin pack
// refers to, so a push is judged from its own bytes. Enforcement — which rules
// apply, what a refusal says, and whether to ask the forge about what the pack
// leaves out or re-sends — belongs to the caller.
//
// # What the answer is worth
//
// A pack carries exactly the objects the receiving side does not already have,
// so an object missing from it is one the receiving side already stores —
// somewhere. Which path it stood at is exactly what the pack does not say: the
// sender chooses what to leave out, and an object the receiving side holds is
// left out whatever path the new tree gives it. Three consequences, all of them
// deliberate, and all of them one-sided the safe way:
//
//   - When a new commit's parent is not in the pack — the normal shape of a push
//     to a new branch — there is no pre-image to diff against, so the whole tree
//     is enumerated and the answer OVER-REPORTS: a first push to a new branch
//     reports every path it carries, and a push that edits one file in a
//     directory reports that directory's other files too. Over-reporting is safe
//     for a deny rule and under-reporting is not, which is why it is the
//     fallback.
//   - A directory whose tree object is not in the pack is reported as ONE
//     opaque entry at its own path (Change.Opaque), never skipped. Its contents
//     are a tree the receiving side stores, but nothing in the pack
//     distinguishes a directory the push left alone from one it moved onto that
//     path, copied there from an earlier push, or restored from an older
//     revision — and skipping it let any of those place anything at any path
//     unread. Only a diff against a parent the pack carries skips a subtree,
//     because only there does an unchanged object id at the same name prove it
//     untouched. Everywhere else the entry carries its object id (Change.OID)
//     and the answer names the commits the push builds on (Result.Bases), so a
//     caller that can read those commits' trees from the receiving side can
//     prove an entry unchanged the same way.
//   - A pack is not always only what is new. git leaves out what is reachable
//     from the tips the receiving side advertises that the sender also has, so
//     a sender holding none of them — a clone taken before its branch moved on
//     — re-sends its history, and that history's first commit is enumerated
//     whole. Result.Settle takes out the commits a caller says the receiving
//     side already holds.
//   - A removal is invisible. The enumerated case has no pre-image to compare
//     against, so this package reports what a push INTRODUCES, not what it takes
//     away.
//
// Everything else that would make the answer a guess is a refusal:
// ErrUninspectable for a pack that does not carry what an answer needs (a thin
// pack's delta bases, a ref whose commit is not in the pack, more objects or
// bytes than the ceilings below allow) or that git would read differently from
// this package (a commit carrying a header git's own parser stops before), and
// a plain error for a malformed or hostile one.
//
// # What one inspection costs
//
// The ceilings below bound one inspection's heap, beyond the body the caller
// buffered: inflated objects up to maxInflatedBytes (128 MiB), with every
// blob's content released when the parse ends, so a Result keeps only trees,
// commits and tags; bookkeeping of at most 160 bytes an object (115 measured),
// about 30 MiB at maxObjects; and a change set of at most maxChanges entries,
// measured at 36 MiB with the tree that names them. About 200 MiB in all, most
// of it the inflation ceiling. How many inspections run at once is the
// caller's to cap; the egress proxy runs one at a time (its scanSlots).
package gitpack

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The ceilings. A hostile push is a small compressed body that asks for an
// enormous amount of memory, so every one of these is a hard refusal rather than
// a best effort. The caller caps the BODY it buffers (see the push_rules
// inspection ceiling, which clamps at 64 MiB); these cap what that body may
// expand into once inflated.
const (
	// maxCommandSection bounds the pkt-line command section, matching the git
	// broker's own ceiling on the same bytes.
	maxCommandSection = 64 << 10
	// maxObjects bounds the object count the pack header claims. It caps both a
	// lying header's allocation and the per-object bookkeeping — an offset, an
	// object id and the object itself — that parsing keeps for the whole pack.
	// Real pushes sit far below it: a single-commit push of a large monorepo's
	// whole tree is under 100,000 objects, and 200,000 objects inside the 64 MiB
	// body ceiling average 335 compressed bytes each, so what the ceiling refuses
	// is a pack of near-empty objects shaped to cost bookkeeping (#250).
	maxObjects = 200_000
	// maxObjectBytes bounds one inflated object, and with it one delta result.
	maxObjectBytes = 32 << 20
	// maxInflatedBytes bounds every inflated object together. A 64 MiB pack of
	// pathologically compressible content would otherwise inflate to gigabytes.
	maxInflatedBytes = 128 << 20
	// maxChanges bounds the reported change set, which the whole-tree
	// enumeration above can make much larger than the number of edited files.
	maxChanges = 200_000
	// maxTreeDepth bounds directory nesting. Real trees are shallow; a pack that
	// claims otherwise is trying to exhaust the stack.
	maxTreeDepth = 64
	// maxTreeNodes bounds how many tree ENTRIES one inspection walks, which
	// maxTreeDepth does not: trees are a DAG, so d levels that each name the
	// level below b times describe b^d paths in a few kilobytes of objects.
	// Entries rather than expansions, so that a wide tree costs what its width
	// says. The headroom over maxChanges is a factor, not an order: git cannot
	// store an empty directory, so an ordinary repository spends a few entries
	// per path it reports — binary fan-out with one file per leaf directory is
	// the dense case, near three — and maxChanges refuses first. What reaches
	// this ceiling instead is a shape spending many entries per path, which
	// means long single-child chains.
	maxTreeNodes = 5 * maxChanges
	// maxPeel bounds tag-to-tag chasing when a push updates a tag ref.
	maxPeel = 8
	// maxVarintBytes bounds the length of a pack's variable-length integers.
	maxVarintBytes = 10
)

// ErrUninspectable reports that the request is well formed but does not carry
// what an answer would need. It is a first-class result, not a failure: the
// agent images clone shallow, so a push whose bases live on the remote is
// ordinary, and the broker answers it by advertising no-thin rather than by
// fetching those bases.
var ErrUninspectable = errors.New("gitpack: the push cannot be inspected from its own bytes")

// Change is one path a push introduces, at the mode and size the pushed tree
// gives it.
type Change struct {
	// Path is slash-separated and relative to the repository root. It is ""
	// only for an uncarried root tree: a commit whose whole tree the receiving
	// side already stores.
	Path string
	// Mode is the tree entry's mode verbatim — "100644", "100755" for an
	// executable, "120000" for a symlink, "160000" for a submodule pointer — so a
	// caller can act on the difference. A directory the pack does not carry is
	// reported as ModeUncarried; see Opaque.
	Mode string
	// Size is the blob's size in bytes, or -1 when the pack does not carry the
	// blob: a submodule pointer, an uncarried directory, or content the
	// receiving side already stores.
	// A size rule must DECIDE what -1 means rather than compare it, because -1
	// passes every "is this under the limit" test by accident.
	Size int64
	// OID is the object id the tree entry names — the blob, the submodule's
	// commit, or for an uncarried directory its tree. Object ids are content
	// addresses, so an entry whose mode and OID match the ones the same path
	// held in a commit the push builds on is unchanged, everything beneath a
	// directory included.
	OID string
}

// Carried reports whether the pack holds the object c names. One it does not
// hold is content the receiving side already stores, at some path.
func (c Change) Carried() bool { return c.Size >= 0 }

// Command is one ref update from the request's command section.
type Command struct {
	// Old and New are object ids in the request's object format; New is all
	// zeros when the command deletes the ref.
	Old, New string
	Ref      string
}

// Result is what one receive-pack request would change.
type Result struct {
	// ObjectFormat is "sha1" or "sha256", as the capability list declared.
	ObjectFormat string
	// Commands are the ref updates, in wire order.
	Commands []Command
	// Changes are the paths the push introduces, sorted and deduplicated.
	Changes []Change
	// Bases are the commits the push builds on: every parent a commit in the
	// pack names that the pack does not carry, sorted and deduplicated. The
	// receiving side holds them if the push is to succeed — but a commit may
	// name ANY object id as its parent, so a caller must establish for itself
	// that a base belongs to the history it trusts before comparing against it.
	// After Settle, a carried commit the caller said the receiving side holds
	// is a base too.
	Bases []string

	// idx is the parsed pack, kept so Settle can answer again without reading
	// the body twice.
	idx *index
}

// objectType is a pack object's wire type.
type objectType byte

const (
	objCommit   objectType = 1
	objTree     objectType = 2
	objBlob     objectType = 3
	objTag      objectType = 4
	objOfsDelta objectType = 6
	objRefDelta objectType = 7
)

// label is the type's name as it appears in the bytes an object id hashes over.
func (t objectType) label() string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

// hashFormat is one object-id format: everything in a pack that is measured in
// object ids — ref deltas, tree entries, commit headers — is this wide.
type hashFormat struct {
	name string
	size int
	new  func() hash.Hash
}

var (
	sha1Format   = hashFormat{name: "sha1", size: sha1.Size, new: sha1.New}
	sha256Format = hashFormat{name: "sha256", size: sha256.Size, new: sha256.New}
)

// Inspect reads a buffered git-receive-pack request body — the pkt-line command
// section, then the packfile — and reports what the push would change. The
// caller is expected to have bounded the body before buffering it.
//
// On any error the Result is empty: a partial answer read as a whole one is the
// failure this package exists to avoid.
func Inspect(body []byte) (Result, error) {
	cmds, caps, rest, err := readCommandSection(body)
	if err != nil {
		return Result{}, err
	}
	format, err := objectFormat(caps)
	if err != nil {
		return Result{}, err
	}
	if slices.Contains(strings.Fields(caps), "push-options") {
		if rest, err = skipPktSection(rest); err != nil {
			return Result{}, fmt.Errorf("gitpack: push-options section: %w", err)
		}
	}
	res := Result{ObjectFormat: format.name, Commands: cmds}
	if len(rest) == 0 {
		// No pack at all. That is an answer for a push that only deletes refs,
		// and a refusal for anything else: nothing about the new content was read.
		if i := slices.IndexFunc(cmds, func(c Command) bool { return !isZeroOID(c.New) }); i >= 0 {
			return Result{}, fmt.Errorf("%w: %s updates a ref and the request carries no packfile",
				ErrUninspectable, cmds[i].Ref)
		}
		return res, nil
	}
	idx, err := parsePack(rest, format)
	if err != nil {
		return Result{}, err
	}
	if err := idx.coverCommands(cmds); err != nil {
		return Result{}, err
	}
	if res.Changes, res.Bases, err = idx.changes(nil); err != nil {
		return Result{}, err
	}
	res.idx = idx
	return res, nil
}

// readCommandSection consumes the pkt-line command section — every ref update up
// to and including the flush-pkt — and returns the commands, the capability list
// the FIRST command carries, and the bytes that follow.
//
// Wire shape (protocol v2 leaves push unchanged): each pkt-line opens with four
// hex length digits that count themselves, "0000" is the flush-pkt, and optional
// "shallow <oid>" lines may precede the commands. Anything else — a bad length,
// a signed push certificate, a section over maxCommandSection — is refused
// rather than skipped: the whole guarantee is that what is not understood is not
// waved through.
func readCommandSection(body []byte) (cmds []Command, caps string, rest []byte, err error) {
	for read := 0; ; {
		if len(body) < 4 {
			return nil, "", nil, errors.New("gitpack: truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(body[:4]), 16, 32)
		if err != nil {
			return nil, "", nil, fmt.Errorf("gitpack: malformed pkt-line length %q", body[:4])
		}
		if n == 0 {
			return cmds, caps, body[4:], nil
		}
		if n < 5 || int(n) > len(body) {
			return nil, "", nil, fmt.Errorf("gitpack: unexpected pkt-line length %d in the command section", n)
		}
		if read += int(n); read > maxCommandSection {
			return nil, "", nil, fmt.Errorf("gitpack: command section exceeds %d bytes", maxCommandSection)
		}
		line := string(body[4:n])
		body = body[n:]
		if caps == "" && len(cmds) == 0 {
			if payload, c, found := strings.Cut(line, "\x00"); found {
				line, caps = payload, strings.TrimSuffix(c, "\n")
			}
		}
		cmd, skip, err := parseCommand(strings.TrimSuffix(line, "\n"))
		if err != nil {
			return nil, "", nil, err
		}
		if !skip {
			cmds = append(cmds, cmd)
		}
	}
}

// parseCommand reads one command-section line: "<old-oid> SP <new-oid> SP
// <refname>", or a "shallow <oid>" line, which announces a shallow boundary and
// updates nothing.
func parseCommand(line string) (cmd Command, skip bool, err error) {
	if strings.HasPrefix(line, "shallow ") {
		return Command{}, true, nil
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[2] == "" || !isHex(parts[0]) || !isHex(parts[1]) {
		return Command{}, false, fmt.Errorf("gitpack: unsupported receive-pack command %q", line)
	}
	return Command{Old: parts[0], New: parts[1], Ref: parts[2]}, false, nil
}

// skipPktSection consumes one flush-terminated pkt-line section and returns what
// follows. `git push -o` puts such a section between the commands and the pack;
// reading it as pack bytes would refuse a legitimate push.
func skipPktSection(body []byte) ([]byte, error) {
	for read := 0; ; {
		if len(body) < 4 {
			return nil, errors.New("truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(body[:4]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed pkt-line length %q", body[:4])
		}
		if n == 0 {
			return body[4:], nil
		}
		if n < 5 || int(n) > len(body) {
			return nil, fmt.Errorf("unexpected pkt-line length %d", n)
		}
		if read += int(n); read > maxCommandSection {
			return nil, fmt.Errorf("section exceeds %d bytes", maxCommandSection)
		}
		body = body[n:]
	}
}

// objectFormat reads the object-id format out of the capability list. An absent
// capability means sha1, which predates the capability; an unrecognized one is
// refused rather than guessed, because guessing the width means reading every
// object id in the pack at the wrong offset and reporting whatever falls out.
func objectFormat(caps string) (hashFormat, error) {
	for _, c := range strings.Fields(caps) {
		v, ok := strings.CutPrefix(c, "object-format=")
		if !ok {
			continue
		}
		switch v {
		case sha1Format.name:
			return sha1Format, nil
		case sha256Format.name:
			return sha256Format, nil
		default:
			return hashFormat{}, fmt.Errorf("gitpack: unsupported object-format %q", v)
		}
	}
	return sha1Format, nil
}

// object is one resolved pack object: its type, its inflated size, and its
// inflated bytes — which a blob gives up once the pack is parsed, because after
// that only its size is ever read (dropBlobContent).
type object struct {
	typ  objectType
	size int64
	data []byte
}

// index is every object a pack carries, keyed by the object id computed from its
// own bytes rather than by anything the pack asserted.
type index struct {
	format hashFormat
	byOID  map[string]object
}

func newIndex(format hashFormat) *index {
	return &index{format: format, byOID: map[string]object{}}
}

// put hashes an object the way git does — "<type> <length>\0" then the bytes —
// and files it under the id that produces.
func (i *index) put(typ objectType, data []byte) string {
	h := i.format.new()
	fmt.Fprintf(h, "%s %d", typ.label(), len(data))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	oid := hex.EncodeToString(h.Sum(nil))
	if _, dup := i.byOID[oid]; !dup {
		i.byOID[oid] = object{typ: typ, size: int64(len(data)), data: data}
	}
	return oid
}

// blobSize is the size of the blob oid names, or -1 when the pack does not carry
// it. See Change.Size.
func (i *index) blobSize(oid string) int64 {
	if o, ok := i.byOID[oid]; ok && o.typ == objBlob {
		return o.size
	}
	return -1
}

// dropBlobContent releases every blob's bytes once no delta can need them as a
// base. A Result outlives the parse — the caller holds it across its forge
// reads — and without this it kept up to maxInflatedBytes of file content live
// that nothing reads again.
func (i *index) dropBlobContent() {
	for oid, o := range i.byOID {
		if o.typ == objBlob {
			o.data = nil
			i.byOID[oid] = o
		}
	}
}

// packReader walks a packfile. The reader is a *bytes.Reader so that inflating
// an object leaves the position exactly at the next object's header: zlib reads
// through an io.ByteReader without the buffering that would over-read past the
// stream it was given.
type packReader struct {
	br       *bytes.Reader
	idx      *index
	zr       io.ReadCloser
	inflated int64
	// atOffset holds every offset an object header began at, so an offset delta
	// can only name a base the pack actually laid out there, and maps it to the
	// object's id once resolved — "" until then. One map rather than two: it is
	// per-object bookkeeping, which maxObjects multiplies.
	atOffset map[int64]string
	// pending are deltas whose base is not resolved yet, and the two indexes
	// that wake them: a ref delta may name a base that appears LATER in the pack,
	// and an offset delta's base may itself be such a delta.
	pending []pendingDelta
	waitOID map[string][]int
	waitOff map[int64][]int
}

// pendingDelta is one unresolved delta: where it sits, what it needs, and the
// instructions it will replay once that base exists.
type pendingDelta struct {
	off     int64
	baseOID string // ref delta
	baseOff int64  // offset delta; -1 for a ref delta
	delta   []byte
}

// parsePack reads a PACK v2 stream and returns every object in it, resolving
// deltas against bases within the same pack.
func parsePack(pack []byte, format hashFormat) (*index, error) {
	if len(pack) < 12+format.size {
		return nil, errors.New("gitpack: packfile is shorter than its own header and trailer")
	}
	if string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("gitpack: expected a packfile, found %s", strconv.QuoteToASCII(string(pack[:4])))
	}
	if v := binary.BigEndian.Uint32(pack[4:8]); v != 2 {
		return nil, fmt.Errorf("gitpack: unsupported pack version %d", v)
	}
	// The trailer is a checksum over everything before it, so verifying it first
	// turns every truncation and every flipped byte into one error here rather
	// than into a plausible-looking partial answer later.
	body, want := pack[:len(pack)-format.size], pack[len(pack)-format.size:]
	h := format.new()
	_, _ = h.Write(body)
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return nil, fmt.Errorf("gitpack: packfile checksum is %x, the trailer says %x (truncated or corrupt)", got, want)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	if count > maxObjects {
		return nil, fmt.Errorf("%w: the pack claims %d objects, more than the %d ceiling",
			ErrUninspectable, count, maxObjects)
	}
	p := &packReader{
		br: bytes.NewReader(body), idx: newIndex(format),
		atOffset: map[int64]string{},
		waitOID:  map[string][]int{}, waitOff: map[int64][]int{},
	}
	if _, err := p.br.Seek(12, io.SeekStart); err != nil {
		return nil, err
	}
	for range count {
		if err := p.object(); err != nil {
			return nil, err
		}
	}
	if p.zr != nil {
		_ = p.zr.Close()
	}
	if n := p.br.Len(); n != 0 {
		return nil, fmt.Errorf("gitpack: %d bytes follow the pack's %d objects", n, count)
	}
	if err := p.unresolved(); err != nil {
		return nil, err
	}
	p.idx.dropBlobContent()
	return p.idx, nil
}

// object reads one pack object: its header, then its inflated payload, then
// either files it or queues it behind the base it deltas against.
func (p *packReader) object() error {
	off := p.pos()
	p.atOffset[off] = ""
	typ, size, err := p.header()
	if err != nil {
		return err
	}
	switch typ {
	case objCommit, objTree, objBlob, objTag:
		data, err := p.inflate(off, size)
		if err != nil {
			return err
		}
		return p.settle(off, p.idx.put(typ, data))
	case objOfsDelta:
		back, err := p.offsetEncoding()
		if err != nil {
			return err
		}
		base := off - back
		if _, begun := p.atOffset[base]; back <= 0 || base < 12 || !begun {
			return fmt.Errorf("gitpack: the delta at %d names a base at %d, where no object begins", off, base)
		}
		data, err := p.inflate(off, size)
		if err != nil {
			return err
		}
		p.park(pendingDelta{off: off, baseOff: base, delta: data})
		if oid := p.atOffset[base]; oid != "" {
			return p.settle(base, oid)
		}
		return nil
	case objRefDelta:
		raw := make([]byte, p.idx.format.size)
		if _, err := io.ReadFull(p.br, raw); err != nil {
			return fmt.Errorf("gitpack: truncated delta base id at %d: %w", off, err)
		}
		data, err := p.inflate(off, size)
		if err != nil {
			return err
		}
		baseOID := hex.EncodeToString(raw)
		p.park(pendingDelta{off: off, baseOID: baseOID, baseOff: -1, delta: data})
		if _, ok := p.idx.byOID[baseOID]; ok {
			return p.settle(-1, baseOID)
		}
		return nil
	}
	return fmt.Errorf("gitpack: unknown object type %d at offset %d", typ, off)
}

// park registers an unresolved delta under whatever it is waiting for.
func (p *packReader) park(d pendingDelta) {
	p.pending = append(p.pending, d)
	i := len(p.pending) - 1
	if d.baseOff >= 0 {
		p.waitOff[d.baseOff] = append(p.waitOff[d.baseOff], i)
		return
	}
	p.waitOID[d.baseOID] = append(p.waitOID[d.baseOID], i)
}

// baseOID names the object a delta needs, once that object is resolved.
func (p *packReader) baseOID(d pendingDelta) (string, bool) {
	if d.baseOff >= 0 {
		oid := p.atOffset[d.baseOff]
		return oid, oid != ""
	}
	_, ok := p.idx.byOID[d.baseOID]
	return d.baseOID, ok
}

// settle files a resolved object and replays every delta that was waiting on it,
// and everything those in turn release. The worklist is deliberate: a delta chain
// is as long as a pack cares to make it, and recursion here would hand a hostile
// pack the stack. off is -1 when the object's position does not matter, which is
// how a ref delta that names an already-resolved base kicks itself off.
func (p *packReader) settle(off int64, oid string) error {
	if off >= 0 {
		p.atOffset[off] = oid
	}
	for work := p.take(off, oid); len(work) > 0; {
		i := work[len(work)-1]
		work = work[:len(work)-1]
		d := p.pending[i]
		base, ok := p.baseOID(d)
		if !ok {
			return fmt.Errorf("gitpack: internal: the delta at %d was woken without its base", d.off)
		}
		b := p.idx.byOID[base]
		data, err := applyDelta(b.data, d.delta)
		if err != nil {
			return fmt.Errorf("gitpack: the delta at offset %d: %w", d.off, err)
		}
		p.pending[i].delta = nil // the instructions are spent; only the result is held
		if err := p.charge(int64(len(data))); err != nil {
			return err
		}
		resolved := p.idx.put(b.typ, data)
		p.atOffset[d.off] = resolved
		work = append(work, p.take(d.off, resolved)...)
	}
	return nil
}

// take removes and returns the deltas parked behind one just-resolved object.
func (p *packReader) take(off int64, oid string) []int {
	work := append([]int(nil), p.waitOff[off]...)
	delete(p.waitOff, off)
	work = append(work, p.waitOID[oid]...)
	delete(p.waitOID, oid)
	return work
}

// unresolved turns whatever is still waiting into the refusal that says so. A
// ref delta against a base the pack does not carry is the ordinary thin-pack
// case; a cycle of ref deltas ends here too, because neither end can ever be
// hashed into existence.
func (p *packReader) unresolved() error {
	for oid := range p.waitOID {
		return fmt.Errorf("%w: a delta needs base object %s, which is not in the pack", ErrUninspectable, oid)
	}
	for off := range p.waitOff {
		return fmt.Errorf("%w: a delta needs the object at offset %d, which never resolved", ErrUninspectable, off)
	}
	return nil
}

// pos is the reader's current offset into the pack.
func (p *packReader) pos() int64 { return p.br.Size() - int64(p.br.Len()) }

// charge books inflated bytes against the whole-pack budget. A delta result is
// booked after it is reconstructed rather than before, so the budget can be
// overshot by at most one object — which maxObjectBytes bounds.
func (p *packReader) charge(n int64) error {
	if p.inflated += n; p.inflated > maxInflatedBytes {
		return fmt.Errorf("%w: the pack inflates past the %d-byte ceiling", ErrUninspectable, int64(maxInflatedBytes))
	}
	return nil
}

// header reads one object header: a type and the size of the payload that
// follows, little-endian in seven-bit groups.
func (p *packReader) header() (objectType, int64, error) {
	b, err := p.br.ReadByte()
	if err != nil {
		return 0, 0, fmt.Errorf("gitpack: truncated object header: %w", err)
	}
	typ := objectType((b >> 4) & 7)
	size := int64(b & 0x0f)
	for shift, n := uint(4), 1; b&0x80 != 0; shift, n = shift+7, n+1 {
		if n >= maxVarintBytes {
			return 0, 0, errors.New("gitpack: object header size is not a sane varint")
		}
		if b, err = p.br.ReadByte(); err != nil {
			return 0, 0, fmt.Errorf("gitpack: truncated object header: %w", err)
		}
		size |= int64(b&0x7f) << shift
		if size < 0 || size > maxObjectBytes {
			return 0, 0, fmt.Errorf("gitpack: object header declares more than the %d-byte ceiling", int64(maxObjectBytes))
		}
	}
	return typ, size, nil
}

// offsetEncoding reads git's offset encoding: base-128, most significant group
// first, each continuation implicitly one larger.
func (p *packReader) offsetEncoding() (int64, error) {
	b, err := p.br.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("gitpack: truncated delta offset: %w", err)
	}
	off := int64(b & 0x7f)
	for n := 1; b&0x80 != 0; n++ {
		if n >= maxVarintBytes {
			return 0, errors.New("gitpack: delta offset is not a sane varint")
		}
		if b, err = p.br.ReadByte(); err != nil {
			return 0, fmt.Errorf("gitpack: truncated delta offset: %w", err)
		}
		off = ((off + 1) << 7) | int64(b&0x7f)
		if off < 0 || off > p.br.Size() {
			return 0, errors.New("gitpack: delta offset runs off the pack")
		}
	}
	return off, nil
}

// inflate reads one zlib stream and insists it produce EXACTLY the number of
// bytes the object header declared. Both directions matter: a short read would
// be a plausible-looking object, and a long one would mean the next object's
// header is not where the pack said it is.
func (p *packReader) inflate(off, declared int64) ([]byte, error) {
	if declared > maxObjectBytes {
		return nil, fmt.Errorf("gitpack: the object at %d declares more than the %d-byte ceiling",
			off, int64(maxObjectBytes))
	}
	if err := p.charge(declared); err != nil {
		return nil, err
	}
	if p.zr == nil {
		zr, err := zlib.NewReader(p.br)
		if err != nil {
			return nil, fmt.Errorf("gitpack: the object at %d does not open a zlib stream: %w", off, err)
		}
		p.zr = zr
	} else if err := p.zr.(zlib.Resetter).Reset(p.br, nil); err != nil {
		return nil, fmt.Errorf("gitpack: the object at %d does not open a zlib stream: %w", off, err)
	}
	// Exactly declared bytes, allocated once: io.ReadAll's minimum buffer is
	// 512 bytes, and a pack of near-empty objects kept one per object (#250).
	// charge has already booked declared against maxInflatedBytes.
	data := make([]byte, declared)
	if n, err := io.ReadFull(p.zr, data); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("gitpack: the object at %d declared %d bytes and inflated to %d", off, declared, n)
		}
		return nil, fmt.Errorf("gitpack: inflating the object at %d: %w", off, err)
	}
	// The stream must END here: a byte more means the next object's header is not
	// where the pack said it is, and reading to EOF is what checks the stream's
	// own checksum.
	var extra [1]byte
	switch n, err := io.ReadFull(p.zr, extra[:]); {
	case n > 0:
		return nil, fmt.Errorf("gitpack: the object at %d inflates past the %d bytes it declared", off, declared)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("gitpack: inflating the object at %d: %w", off, err)
	}
	return data, nil
}

// applyDelta replays a git delta against its base.
//
// The declared sizes are assertions, not hints. A delta that copies from outside
// its base, or produces a byte more or less than it said it would, reconstructs
// an object that is not the one the pack promised — and an object that is nearly
// right is the worst outcome available, because it yields a plausible tree that
// a deny rule then evaluates as if it were the truth. Every one of those is an
// error here and no bytes come back with it.
func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	baseSize, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("unreadable base size: %w", err)
	}
	if baseSize != uint64(len(base)) {
		return nil, fmt.Errorf("declares a base size of %d against a %d-byte base", baseSize, len(base))
	}
	want, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("unreadable result size: %w", err)
	}
	if want > maxObjectBytes {
		return nil, fmt.Errorf("declares a %d-byte result, more than the %d-byte ceiling", want, int64(maxObjectBytes))
	}
	out := make([]byte, 0, want)
	for {
		op, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case op&0x80 != 0:
			if out, err = copyFromBase(r, op, base, out); err != nil {
				return nil, err
			}
		case op != 0:
			chunk := make([]byte, op)
			if _, err := io.ReadFull(r, chunk); err != nil {
				return nil, fmt.Errorf("insert of %d bytes: %w", op, err)
			}
			out = append(out, chunk...)
		default:
			return nil, errors.New("instruction 0 is reserved")
		}
		if uint64(len(out)) > want {
			return nil, fmt.Errorf("produced more than the %d bytes it declared", want)
		}
	}
	if uint64(len(out)) != want {
		return nil, fmt.Errorf("declared a result size of %d and produced %d bytes", want, len(out))
	}
	return out, nil
}

// copyFromBase replays one copy instruction: the opcode's low bits say which of
// the offset's four bytes and the size's three bytes are present.
func copyFromBase(r *bytes.Reader, op byte, base, out []byte) ([]byte, error) {
	var offset, size uint64
	for i := range 4 {
		if op&(1<<uint(i)) != 0 {
			b, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("truncated copy offset: %w", err)
			}
			offset |= uint64(b) << (8 * uint(i))
		}
	}
	for i := range 3 {
		if op&(0x10<<uint(i)) != 0 {
			b, err := r.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("truncated copy size: %w", err)
			}
			size |= uint64(b) << (8 * uint(i))
		}
	}
	if size == 0 {
		size = 0x10000 // git's documented shorthand for a 64 KiB copy
	}
	if end := offset + size; end < offset || end > uint64(len(base)) {
		return nil, fmt.Errorf("copies [%d,%d) from a %d-byte base", offset, offset+size, len(base))
	}
	return append(out, base[offset:offset+size]...), nil
}

// isZeroOID reports whether an object id is the all-zero id, which a command
// uses to say "this ref does not exist" on either side of an update.
func isZeroOID(oid string) bool { return oid != "" && strings.Trim(oid, "0") == "" }

// isHex reports whether s is a non-empty run of lowercase hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
