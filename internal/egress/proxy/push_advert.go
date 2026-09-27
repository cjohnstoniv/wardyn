// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Advertising `no-thin` on the brokered receive-pack reference advertisement,
// so a push that content rules will be asked about arrives as a pack that can
// be read on its own.
//
// Why it is needed at all: the agent images clone with --depth 1
// (deploy/images/common/agent-run-lib.sh), so the objects a real push
// deltifies against live on the FORGE and not in the pack the sandbox sends.
// git's send-pack asks for a thin pack by default, and a shallow clone's next
// commit normally puts its root tree — and any edited large blob — on the wire
// as a delta against a base object the pack does not carry. Nothing in the
// request can resolve that base (internal/gitpack answers ErrUninspectable), so
// a content rule that refuses what it cannot read would refuse nearly every
// legitimate push. The protocol already answers this: gitprotocol-capabilities
// says a client MUST NOT send a thin pack when the server advertises no-thin,
// so the broker adds the capability to the advertisement it relays and the
// client packs the bases in itself.
//
// Rejected: having the proxy fetch the missing base objects from the forge.
// That makes the proxy dial out on the run's behalf, which is the one thing the
// broker exists not to do.
//
// FAIL SAFE, NOT CLOSED. Every path that does not find exactly the shape
// rewriteAdvertHead documents relays the advertisement BYTE FOR BYTE. A
// corrupted reference advertisement breaks every push through the broker,
// including runs that have no content rules at all; an un-rewritten one merely
// yields a thin pack, which the enforcement path refuses on its own terms.
//
// Scope: the receive-pack advertisement only. The fetch advertisement
// (service=git-upload-pack) is never touched — a thin FETCH pack is git
// resolving deltas against objects the client already has, which is the
// protocol working, not a blind spot.

const (
	// noThinCap is the capability appended to the advertised list.
	noThinCap = "no-thin"
	// advertServiceLine is the payload (no length prefix) of the pkt-line git's
	// smart-HTTP transport puts ahead of a receive-pack reference advertisement.
	advertServiceLine = "# service=git-receive-pack\n"
	// maxPktLine is git's LARGE_PACKET_MAX: the largest total a 4-hex length
	// prefix may state. A rewrite that would cross it is abandoned rather than
	// truncated or wrapped.
	maxPktLine = 65520
)

// noThinAdvert reports whether THIS git-broker request is the receive-pack
// reference advertisement of a run whose policy carries content rules — the
// only request whose response is rewritten.
func (p *Proxy) noThinAdvert(r *http.Request, rest string) bool {
	return rest == "info/refs" &&
		r.URL.Query().Get("service") == "git-receive-pack" &&
		p.policy.PushRulesSet()
}

// relayNoThinAdvert is relay() for a receive-pack reference advertisement: it
// rewrites the head of the body to advertise no-thin and streams the rest
// untouched. When the head is not the shape it expects, the bytes it read go
// back verbatim and the relay is byte-for-byte what upstream sent.
func relayNoThinAdvert(w http.ResponseWriter, resp *http.Response) {
	head, rewritten := advertWithNoThin(resp)
	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	if rewritten {
		// The rewritten line is longer than the one upstream counted.
		dst.Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(head)         // the advertisement head: rewritten, or verbatim
	_, _ = io.Copy(w, resp.Body) // every ref after it, untouched
}

// advertWithNoThin returns the bytes to write in place of the head of resp's
// body, and whether they differ from what was read.
//
// A non-200 is an error page, not an advertisement. A body still in a
// content-coding is unreadable here: handleGitBroker asks for identity on
// exactly this request instead of forwarding the sandbox's own negotiation,
// but a forge that encodes unasked is relayed rather than read as if it were
// plaintext.
func advertWithNoThin(resp *http.Response) (head []byte, rewritten bool) {
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, false
	}
	return rewriteAdvertHead(resp.Body)
}

// rewriteAdvertHead reads the head of a smart-HTTP receive-pack reference
// advertisement and returns the bytes that replace it, plus whether they
// changed. It consumes exactly three pkt-lines; everything after them is the
// caller's to stream untouched.
//
// The shape it accepts, and nothing else:
//
//	001f# service=git-receive-pack\n
//	0000
//	<len><old-oid> <ref>\0<capability list>\n
//
// The capability list rides the FIRST ref line only, which is why the two
// lines ahead of it are read rather than skipped: reading them is how this
// knows the third line is the one that carries capabilities. An empty
// repository advertises `<zero-oid> capabilities^{}\0<caps>` in that same
// position, so it needs no case of its own.
func rewriteAdvertHead(body io.Reader) (head []byte, rewritten bool) {
	svcRaw, svc, err := readPkt(body)
	head = svcRaw
	if err != nil || string(svc) != advertServiceLine {
		return head, false
	}
	flushRaw, flush, err := readPkt(body)
	head = append(head, flushRaw...)
	if err != nil || flush != nil { // anything but the flush-pkt ends the header
		return head, false
	}
	refRaw, ref, err := readPkt(body)
	if err != nil || ref == nil {
		return append(head, refRaw...), false
	}
	line, ok := capLineWithNoThin(ref)
	if !ok {
		return append(head, refRaw...), false
	}
	return append(head, line...), true
}

// capLineWithNoThin returns the advertisement's first ref line with no-thin
// added to its capability list and its 4-hex length prefix recomputed.
//
// ok=false means "leave this line alone": it carries no capability list at all,
// it already advertises no-thin, or the added capability would push the
// pkt-line past maxPktLine.
func capLineWithNoThin(payload []byte) ([]byte, bool) {
	nul := bytes.IndexByte(payload, 0)
	if nul < 0 {
		return nil, false
	}
	caps := payload[nul+1:]
	// The trailing LF belongs to the pkt-line, not to the capability list: git
	// chomps exactly one before parsing, so a capability appended AFTER it would
	// fold that newline into the last advertised token instead of adding one.
	list := caps
	if n := len(list); n > 0 && list[n-1] == '\n' {
		list = list[:n-1]
	}
	if slices.Contains(strings.Fields(string(list)), noThinCap) {
		return nil, false
	}
	out := make([]byte, 0, len(payload)+len(noThinCap)+1)
	out = append(out, payload[:nul+1]...)
	out = append(out, list...)
	if len(list) > 0 {
		out = append(out, ' ')
	}
	out = append(out, noThinCap...)
	out = append(out, caps[len(list):]...) // the LF, if there was one
	n := len(out) + 4                      // a pkt-line length counts its own 4 digits
	if n > maxPktLine {
		return nil, false
	}
	return append([]byte(fmt.Sprintf("%04x", n)), out...), true
}

// readPkt reads one pkt-line. It returns the bytes it consumed VERBATIM — so a
// caller that gives up can still relay exactly what it took off the wire — and
// the payload those bytes carried. A flush-pkt ("0000") yields a nil payload,
// which is how the caller tells it from an empty one.
func readPkt(r io.Reader) (raw, payload []byte, err error) {
	hdr := make([]byte, 4)
	n, err := io.ReadFull(r, hdr)
	raw = hdr[:n]
	if err != nil {
		return raw, nil, err
	}
	length, err := strconv.ParseUint(string(hdr), 16, 32)
	if err != nil {
		return raw, nil, fmt.Errorf("malformed pkt-line length %q", hdr)
	}
	if length == 0 {
		return raw, nil, nil
	}
	// 0001/0002 (delim / response-end) are protocol v2 punctuation, which a
	// receive-pack advertisement does not use.
	if length < 4 {
		return raw, nil, fmt.Errorf("unexpected pkt-line length %d", length)
	}
	p := make([]byte, length-4)
	n, err = io.ReadFull(r, p)
	raw = append(raw, p[:n]...)
	if err != nil {
		return raw, nil, err
	}
	return raw, p, nil
}
