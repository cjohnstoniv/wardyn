// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Where a run's model credential lives, graded once by the server.
//
// An unconditional static claim — "Minted at launch, injected by the proxy.
// Never written into the sandbox." — is a FALSE ASSURANCE: it is read by the
// person deciding whether a per-user AWS credential may sit inside a
// shared-kernel container, at the moment they decide. So the console repeats
// what this file answers from the model provider the run chose.
package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// modelCredentialResidency is where the MODEL credential of a run lands. The
// vocabulary is deliberately about PLACE, not kind: the rail's job is to tell a
// reviewer whether a credential is inside the sandbox they are about to grant.
type modelCredentialResidency string

const (
	// residencyProxy: late-bound and injected on the wire; the sandbox holds a
	// placeholder at most.
	residencyProxy modelCredentialResidency = "proxy"
	// residencySandbox: a live credential lands inside the sandbox for the run's
	// lifetime: the AWS sign-in's role credentials (SigV4 signs in-process, so
	// it cannot be proxy-injected).
	residencySandbox modelCredentialResidency = "sandbox"
)

// modelCredentialFacts is what preflight publishes about a run's model
// credential: which provider serves it, its kind, and where the credential
// lands. MEMBER-SAFE by construction: an id, a kind and a place carry no host,
// no secret name and no access-portal URL.
type modelCredentialFacts struct {
	Provider  string                   `json:"provider,omitempty"`
	Kind      string                   `json:"kind,omitempty"`
	Residency modelCredentialResidency `json:"residency,omitempty"`
	// bedrockHost is the Bedrock data-plane host when the provider is a Bedrock
	// one, else "". Unexported, so it is never published: it is the autonomy
	// gate's input (bedrockCredGradedAs).
	bedrockHost string
}

// kindResidency is where a provider kind puts each person's credential.
// bedrock_sso materialises the captured session inside the sandbox and the
// in-sandbox SDK mints resident role credentials from it; every other kind is a
// header the proxy sets on the wire (the key and endpoint kinds, the Bedrock
// bearer key, the Claude sign-in's token), so the sandbox holds a placeholder at
// most.
func kindResidency(k types.ModelProviderKind) modelCredentialResidency {
	if k == types.ModelProviderBedrockSSO {
		return residencySandbox
	}
	return residencyProxy
}
