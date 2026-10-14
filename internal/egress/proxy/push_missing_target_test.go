// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// carriedPush is a pack holding one commit (and its empty tree) with a command
// for it plus a command for each extra "ref": "oid" update the pack lacks.
func carriedPush(carriedRef string, extra ...[2]string) string {
	const objCommit, objTree byte = 1, 2
	commit := fmt.Appendf(nil, "tree %s\nauthor T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n\nx\n", gitObjectID("tree", nil))
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = append(pack, packObjHeader(objCommit, int64(len(commit)))...)
	pack = append(pack, storedZlib(commit)...)
	pack = append(pack, packObjHeader(objTree, 0)...)
	pack = append(pack, storedZlib(nil)...)
	sum := sha1.Sum(pack)
	cmds := pkt(zeroOID + " " + gitObjectID("commit", commit) + " " + carriedRef + firstCaps)
	for _, e := range extra {
		cmds += pkt(zeroOID + " " + e[1] + " " + e[0])
	}
	return cmds + "0000" + string(pack) + string(sum[:])
}

// A push whose ref points at a commit the request does not carry stays refused
// (415, the same rule source, nothing minted or forwarded), but the text names
// each such ref and no longer sends the person to a complete clone.
func TestPushRulesNameRefsWhoseTargetTheRequestLacks(t *testing.T) {
	anyBranch := types.RunPolicySpec{
		GitPushAnyBranch: true,
		PushRules:        &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
	}
	for _, tc := range []struct {
		name    string
		body    string
		want    []string
		notWant []string
	}{
		{"a branch created at a commit the forge has", pkt(zeroOID+" "+someOID+" refs/heads/second"+firstCaps) + "0000" + emptyPack(),
			[]string{"refs/heads/second"}, nil},
		{"no pack at all", pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000",
			[]string{"refs/heads/main"}, nil},
		{"one ref carried, one not", carriedPush("refs/heads/carried", [2]string{"refs/heads/absent", someOID}),
			[]string{"refs/heads/absent"}, []string{"refs/heads/carried"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), anyBranch)

			rec := postPush(t, p, tc.body)
			got := rec.Body.String()

			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415 (body %q)", rec.Code, got)
			}
			if !strings.Contains(got, "does not carry the commit these refs point to") {
				t.Errorf("refusal = %q, want the missing-target wording", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("refusal = %q, want it to name %s", got, w)
				}
			}
			for _, w := range append(tc.notWant, "unshallow", "complete clone") {
				if strings.Contains(got, w) {
					t.Errorf("refusal = %q, must not contain %q", got, w)
				}
			}
			if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
				t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBlind)
			}
			if up.mintCalls != 0 || up.gitHits != 0 {
				t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
					up.mintCalls, up.gitHits)
			}
		})
	}
}

// A pack with no objects.
func emptyPack() string {
	pack := binary.BigEndian.AppendUint32([]byte("PACK"), 2)
	pack = binary.BigEndian.AppendUint32(pack, 0)
	sum := sha1.Sum(pack)
	return string(pack) + string(sum[:])
}
