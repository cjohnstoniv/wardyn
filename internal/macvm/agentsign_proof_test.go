// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	gomacho "github.com/blacktop/go-macho"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign"
	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign/machosigntest"
)

// proofEnv is the agent build and the output directory the macOS CI job's signing proof names; the proof tests
// skip without them.
func proofEnv(t *testing.T) (agent, dir string) {
	agent, dir = os.Getenv("WARDYN_SIGN_PROOF_AGENT"), os.Getenv("WARDYN_SIGN_PROOF_DIR")
	if agent == "" || dir == "" {
		t.Skip("WARDYN_SIGN_PROOF_AGENT and WARDYN_SIGN_PROOF_DIR name a real guest agent build and an output directory")
	}
	return agent, dir
}

// TestSignProof signs a real guest agent build for the CI job's codesign checks. With a throwaway identity
// made in DIR/identity, it writes DIR/wardyn/wardyn-guest-agent (signed in this process, re-parsed here: every
// page hash, the identifier, the designated requirement) and DIR/requirement.txt (AgentRequirement, what
// `codesign -d -r-` must print). On macOS it also runs the whole host signer, Wardyn's signer and this Mac's
// codesign checks, and wants the Wardyn signer to pass them.
func TestSignProof(t *testing.T) {
	agent, dir := proofEnv(t)
	cert, err := macvm.AgentSigningIdentity(context.Background(), filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(filepath.Join(dir, "identity", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(kb)
	if blk == nil {
		t.Fatal("key.pem is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(agent)
	if err != nil {
		t.Fatal(err)
	}
	out, err := machosign.Sign(bin, macvm.AgentSigningID, cert, key.(*rsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	gm, err := gomacho.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	cs := gm.CodeSignature()
	if cs == nil || len(cs.CodeDirectories) != 1 || cs.CodeDirectories[0].ID != macvm.AgentSigningID || len(cs.Requirements) != 1 ||
		machosigntest.AppleText(cs.Requirements[0].Detail) != macvm.AgentRequirement(cert) {
		t.Fatalf("the signed build re-parses as %+v", cs)
	}
	cd := cs.CodeDirectories[0]
	for i, slot := range cd.CodeSlots {
		sum := sha256.Sum256(out[i*4096 : min((i+1)*4096, int(cd.Header.CodeLimit))])
		if !bytes.Equal(slot.Hash, sum[:]) {
			t.Fatalf("page %d hash does not verify", i)
		}
	}
	t.Logf("%d bytes signed: %d pages, code limit %#x, exec segment %#x", len(out), len(cd.CodeSlots), cd.Header.CodeLimit, cd.Header.ExecSegLimit)
	if err := os.MkdirAll(filepath.Join(dir, "wardyn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wardyn", "wardyn-guest-agent"), out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requirement.txt"), []byte(macvm.AgentRequirement(cert)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	sa, err := (&macvm.HostAgentSigner{Dir: filepath.Join(dir, "identity")}).SignAgent(context.Background(), agent)
	if err != nil {
		t.Fatalf("the host signer on this Mac: %v", err)
	}
	defer sa.Done()
	if sa.Signer != macvm.AgentSignerWardyn || len(sa.Errors) != 0 {
		t.Fatalf("the host signer on this Mac used %q after %q, want the Wardyn signer to pass codesign's checks", sa.Signer, sa.Errors)
	}
}

// TestSignProofCodesignFallback runs the codesign fallback for real on macOS (a temporary keychain on the
// search list, the requirement as -r= text) with TestSignProof's identity, and writes
// DIR/codesign/wardyn-guest-agent. It settles whether codesign signs with an identity this Mac does not trust.
func TestSignProofCodesignFallback(t *testing.T) {
	agent, dir := proofEnv(t)
	if runtime.GOOS != "darwin" {
		t.Skip("codesign runs on macOS only")
	}
	tmp := t.TempDir()
	cp, err := (&macvm.HostAgentSigner{Dir: filepath.Join(dir, "identity")}).SignWithCodesign(context.Background(), agent, tmp)
	if err != nil {
		t.Fatalf("the codesign fallback on this Mac: %v", err)
	}
	b, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "codesign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codesign", "wardyn-guest-agent"), b, 0o755); err != nil {
		t.Fatal(err)
	}
}
