// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// deepTreePush is a new branch whose one file sits under levels nested directories ("d/d/…/f").
func deepTreePush(ref string, levels int) []byte {
	const objCommit, objTree, objBlob byte = 1, 2, 3
	entry := func(mode, name, oid string) []byte {
		raw, _ := hex.DecodeString(oid)
		return append([]byte(mode+" "+name+"\x00"), raw...)
	}
	type object struct {
		typ  byte
		data []byte
	}
	blob := []byte("x\n")
	tree := entry("100644", "f", gitObjectID("blob", blob))
	objs := []object{{objBlob, blob}, {objTree, tree}}
	for range levels {
		tree = entry("40000", "d", gitObjectID("tree", tree))
		objs = append(objs, object{objTree, tree})
	}
	commit := fmt.Appendf(nil, "tree %s\nauthor T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n\nx\n", gitObjectID("tree", tree))
	objs = append(objs, object{objCommit, commit})

	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, uint32(len(objs)))
	for _, o := range objs {
		pack = append(pack, packObjHeader(o.typ, int64(len(o.data)))...)
		pack = append(pack, storedZlib(o.data)...)
	}
	sum := sha1.Sum(pack)
	cmd := zeroOID + " " + gitObjectID("commit", commit) + " " + ref + firstCaps
	return append([]byte(pkt(cmd)+"0000"), append(pack, sum[:]...)...)
}

// A push whose trees nest past the inspector's depth ceiling is refused as a ceiling (413, the
// gitpack-big rule source) with a remedy about nesting, never the reclone advice for a thin pack.
func TestPushRulesRefuseTreesNestedTooDeep(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec("nonexistent/**"))

	rec := postPush(t, p, string(deepTreePush(BranchNSPrefix(p.runID)+"work", 70)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "nested directories") || strings.Contains(body, "unshallow") {
		t.Errorf("refusal = %q, want the nesting remedy and no --unshallow advice", body)
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBig+`"`) {
		t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBig)
	}
	if up.mintCalls != 0 || up.gitHits != 0 {
		t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
			up.mintCalls, up.gitHits)
	}
}
