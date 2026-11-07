// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/x509"
	"errors"
)

// AgentSigningID is the identifier the guest agent is signed with.
const AgentSigningID = "com.wardyn.guest-agent"

// How the guest agent a tools update pushed is signed (ToolsRecord.AgentSignature).
const (
	AgentSignedThisMac = "this_mac"
	AgentSignedAdHoc   = "ad_hoc"
)

// AgentSigningIdentity reads this Mac's guest agent signing identity in dir.
func AgentSigningIdentity(ctx context.Context, dir string) (*x509.Certificate, error) {
	return nil, nil
}

// AgentRequirement is the designated requirement of an agent signed with cert.
func AgentRequirement(cert *x509.Certificate) string { return "" }

// AgentSigner signs a copy of the guest agent.
type AgentSigner interface {
	SignAgent(ctx context.Context, path string) (SignedAgent, error)
}

// SignedAgent is a signed copy of the guest agent.
type SignedAgent struct {
	Path string
	Cert []byte
	Done func()
}

// HostAgentSigner signs the guest agent on this Mac.
type HostAgentSigner struct {
	Dir  string
	Exec func(ctx context.Context, argv []string) ([]byte, error)
}

// SignAgent signs a copy of the agent at path.
func (s *HostAgentSigner) SignAgent(ctx context.Context, path string) (SignedAgent, error) {
	return SignedAgent{}, errors.New("not yet")
}
