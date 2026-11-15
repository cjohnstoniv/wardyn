// Copyright 2026 The Wardyn Authors
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

// octopusPush is a new branch whose one commit names parents parent commits the pack does not carry.
func octopusPush(ref string, parents int) []byte {
	const objCommit, objTree, objBlob byte = 1, 2, 3
	blob := []byte("x\n")
	blobID, _ := hex.DecodeString(gitObjectID("blob", blob))
	tree := append([]byte("100644 f\x00"), blobID...)
	commit := fmt.Appendf(nil, "tree %s\n", gitObjectID("tree", tree))
	for i := range parents {
		commit = fmt.Appendf(commit, "parent %040x\n", i+1)
	}
	commit = append(commit, "author T <t@example.com> 1767225600 +0000\ncommitter T <t@example.com> 1767225600 +0000\n\nx\n"...)

	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, 3)
	for _, o := range []struct {
		typ  byte
		data []byte
	}{{objBlob, blob}, {objTree, tree}, {objCommit, commit}} {
		pack = append(pack, packObjHeader(o.typ, int64(len(o.data)))...)
		pack = append(pack, storedZlib(o.data)...)
	}
	sum := sha1.Sum(pack)
	cmd := zeroOID + " " + gitObjectID("commit", commit) + " " + ref + firstCaps
	return append([]byte(pkt(cmd)+"0000"), append(pack, sum[:]...)...)
}

// A push whose commit names more parents than the inspector keeps is refused as a ceiling (413, the
// gitpack-big rule source) with fixed text and a remedy about merging fewer branches, and reaches no
// forge; a commit at the ceiling is inspected as usual.
func TestPushRulesRefuseACommitWithTooManyParents(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec("nonexistent/**"))

	rec := postPush(t, p, string(octopusPush(BranchNSPrefix(p.runID)+"work", 1025)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
	}
	const want = "wardyn: cannot enforce push content rules on this push: gitpack: the push exceeds an inspection " +
		"ceiling: a commit with more than 1024 parents\nmerge fewer branches in one commit"
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Errorf("refusal = %q, want it to contain %q", body, want)
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBig+`"`) {
		t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBig)
	}
	if up.mintCalls != 0 || up.gitHits != 0 {
		t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
			up.mintCalls, up.gitHits)
	}

	if rec := postPush(t, p, string(octopusPush(BranchNSPrefix(p.runID)+"work", 1024))); rec.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("a commit with 1024 parents = 413 %q, want it inspected", rec.Body.String())
	}
}
