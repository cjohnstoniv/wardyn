// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"errors"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsResult says what answer gave, for the relay's log.
type dnsResult struct {
	name    string // validated name, or "" when refused for its spelling
	qtype   string
	addr    [4]byte
	given   bool   // an A answer was given
	refused string // "name" (bad spelling) or "full" (table full of live names)
	evicted string
}

// answer builds the reply to one guest DNS query from the name table alone:
// nothing is ever looked up upstream, so DNS carries nothing out of the VM.
// An A/IN question gets a synthetic address (TTL 60); every other type gets
// NOERROR with no answer; a name that is not a valid hostname, or a full table,
// gets REFUSED. A query that does not parse, or asks more or less than one
// question, gets no reply at all.
func answer(t *nameTable, q []byte) ([]byte, dnsResult, error) {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil, dnsResult{}, err
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil, dnsResult{}, err
	}
	if h.Response || len(qs) != 1 {
		return nil, dnsResult{}, errors.New("want one question")
	}
	question := qs[0]
	res := dnsResult{qtype: question.Type.String()}
	hdr := dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true}

	// Name.String does not escape label bytes, so a label may carry CR, LF or a
	// space: the spelling check below is what keeps them off the CONNECT line.
	name := strings.TrimSuffix(strings.ToLower(question.Name.String()), ".")
	if !validName(name) {
		res.refused = "name"
		hdr.RCode = dnsmessage.RCodeRefused
		return build(hdr, question, nil), res, nil
	}
	res.name = name
	if question.Type == dnsmessage.TypeA && question.Class == dnsmessage.ClassINET {
		addr, evicted, ok := t.resolve(name)
		if !ok {
			res.refused = "full"
			hdr.RCode = dnsmessage.RCodeRefused
			return build(hdr, question, nil), res, nil
		}
		res.addr, res.given, res.evicted = addr, true, evicted
		return build(hdr, question, &dnsmessage.AResource{A: addr}), res, nil
	}
	return build(hdr, question, nil), res, nil
}

func build(hdr dnsmessage.Header, q dnsmessage.Question, a *dnsmessage.AResource) []byte {
	b := dnsmessage.NewBuilder(nil, hdr)
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	if a != nil {
		_ = b.StartAnswers()
		_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: uint32(answerTTL.Seconds())}, *a)
	}
	out, _ := b.Finish()
	return out
}
