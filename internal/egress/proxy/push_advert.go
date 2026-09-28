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

// Advertises `no-thin` on the brokered receive-pack reference advertisement:
// agent images clone --depth 1, so a real push's thin-pack deltas reference
// base objects the pack never carries, which a content rule can't resolve
// (ErrUninspectable) and would refuse — gitprotocol-capabilities lets the
// server force this off via no-thin. Rejected: having the proxy fetch the
// missing bases itself (that would dial out on the run's behalf).
//
// FAIL SAFE, NOT CLOSED: anything not matching rewriteAdvertHead's exact shape
// is relayed byte-for-byte. Scope is receive-pack only — a thin FETCH pack is
// normal delta resolution, not a blind spot.

const (
	noThinCap = "no-thin"
	// advertServiceLine is the pkt-line payload (no length prefix) ahead of a receive-pack advertisement.
	advertServiceLine = "# service=git-receive-pack\n"
	// maxPktLine is git's LARGE_PACKET_MAX; an over-limit rewrite is abandoned, not truncated or wrapped.
	maxPktLine = 65520
)

// noThinAdvert is true only for a receive-pack advertisement under content
// rules — the only response this rewrites.
func (p *Proxy) noThinAdvert(r *http.Request, rest string) bool {
	return rest == "info/refs" &&
		r.URL.Query().Get("service") == "git-receive-pack" &&
		p.policy.PushRulesSet()
}

// relayNoThinAdvert rewrites the head to add no-thin and streams the rest
// untouched; an unrecognized head is relayed byte-for-byte instead.
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

// advertWithNoThin returns the bytes to write in place of resp's head, and
// whether they changed. Only a 200 with no/identity content-coding is read.
func advertWithNoThin(resp *http.Response) (head []byte, rewritten bool) {
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, false
	}
	return rewriteAdvertHead(resp.Body)
}

// rewriteAdvertHead reads and returns the head of a smart-HTTP receive-pack
// advertisement (exactly three pkt-lines), rewritten if it matches:
//
//	001f# service=git-receive-pack\n
//	0000
//	<len><old-oid> <ref>\0<capability list>\n
//
// An empty repository advertises `<zero-oid> capabilities^{}\0<caps>` in the
// same position, needing no case of its own.
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

// capLineWithNoThin adds no-thin to payload's capability list and recomputes
// its length prefix; ok=false leaves the line alone (no list, already
// advertised, or would exceed maxPktLine).
func capLineWithNoThin(payload []byte) ([]byte, bool) {
	nul := bytes.IndexByte(payload, 0)
	if nul < 0 {
		return nil, false
	}
	caps := payload[nul+1:]
	// The trailing LF belongs to the pkt-line, not the list: git chomps one.
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

// readPkt reads one pkt-line, returning bytes consumed VERBATIM (so a caller
// that gives up can still relay them) and the payload; flush-pkt ("0000")
// yields a nil payload, distinguishing it from empty.
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
	// 0001/0002 are protocol v2 punctuation; not used here.
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
