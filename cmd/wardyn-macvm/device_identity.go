// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
)

// identitySource lists the device identities this Mac's MDM installed.
type identitySource interface {
	Identities() ([]deviceIdentity, error)
}

// deviceIdentity is one MDM identity: its certificate chain, leaf first, and its private key's signature. The
// key never leaves the keychain; Sign is the only use the helper makes of it.
type deviceIdentity struct {
	Chain [][]byte
	sign  func(msg []byte) ([]byte, error)
}

// Sign signs msg with the identity's key (ECDSA ASN.1 or RSA PKCS#1 v1.5, over SHA-256 of msg).
func (i deviceIdentity) Sign(msg []byte) ([]byte, error) {
	if i.sign == nil {
		return nil, errors.New("the identity cannot sign")
	}
	return i.sign(msg)
}

// errKeyType is a Sign refusal for a key the proof cannot use (not EC or RSA); the identity is skipped.
var errKeyType = errors.New("the identity's key is not an EC P-256 or RSA key")

// identityReport is the output: the same JSON as runnerwire.DeviceCertReport, which this helper does not import.
type identityReport struct {
	Proofs []identityProof `json:"proofs"`
}

type identityProof struct {
	Chain     [][]byte  `json:"chain"`
	SignedAt  time.Time `json:"signed_at"`
	Signature []byte    `json:"signature"`
}

// cmdDeviceIdentity prints a proof for each device identity the Mac's MDM installed. It takes typed fields and
// builds the message itself, from the fingerprint of the runner key in the state directory, so every signature
// it makes starts devicecert.Prefix: it cannot be made to sign bytes of the caller's choosing.
func cmdDeviceIdentity(args []string, d deps) int {
	if err := printDeviceIdentity(args, d); err != nil {
		fmt.Fprintf(d.stderr, "%s%v\n", helperErrorPrefix, err)
		return 2
	}
	return 0
}

// maxSkipLines is how many skipped identities get a line of their own; the runner reads at most 4096 bytes of stderr.
const maxSkipLines = 8

// helperErrorPrefix starts the helper's own error line; the runner reads it back (wardyn-runnerd's helperFailurePrefix).
const helperErrorPrefix = "wardyn-macvm: "

func printDeviceIdentity(args []string, d deps) error {
	fs := flag.NewFlagSet("device-identity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", "", "the runner's state directory")
	orgSHA := fs.String("org-sha256", "", "the organisation URL's SHA-256, 64 lower-case hex characters")
	signedAtFlag := fs.String("signed-at", "", "the time to sign, RFC 3339")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("device-identity: %w (usage: device-identity --state-dir <dir> --org-sha256 <hex> --signed-at <time>)", err)
	}
	if fs.NArg() != 0 || *stateDir == "" {
		return errors.New("device-identity: usage: device-identity --state-dir <dir> --org-sha256 <hex> --signed-at <time>")
	}
	signedAt, err := time.Parse(time.RFC3339, *signedAtFlag)
	if err != nil {
		return fmt.Errorf("device-identity: --signed-at: %w", err)
	}
	signedAt = signedAt.UTC().Truncate(time.Second)
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	if skew := now().Sub(signedAt); skew > devicecert.MaxSkew || -skew > devicecert.MaxSkew {
		return fmt.Errorf("device-identity: --signed-at is more than %s from this Mac's clock", devicecert.MaxSkew)
	}
	fingerprint, err := devicecert.FingerprintFromStateDir(*stateDir)
	if err != nil {
		return fmt.Errorf("device-identity: %w", err)
	}
	msg, err := devicecert.Message(fingerprint, *orgSHA, signedAt)
	if err != nil {
		return fmt.Errorf("device-identity: --org-sha256: %w", err)
	}
	src := d.identities
	if src == nil {
		src = keychainIdentities{}
	}
	ids, err := src.Identities()
	if err != nil {
		return fmt.Errorf("device-identity: %w", err)
	}
	report := identityReport{Proofs: []identityProof{}}
	refused := 0
	// The cap counts proofs, so an identity that cannot be reported (a chain out of bounds, a key of another
	// type, a key that refuses) never uses one of the places.
	for _, id := range ids {
		if len(report.Proofs) == devicecert.MaxProofs {
			break
		}
		if len(id.Chain) == 0 || len(id.Chain) > devicecert.MaxChainLen || !derWithinBound(id.Chain) {
			continue
		}
		sig, err := id.Sign(msg)
		if errors.Is(err, errKeyType) {
			continue
		}
		if err != nil {
			// One key that will not sign must not cost the others their proof: say which, and go on.
			// At most maxSkipLines lines, then one count, so the runner's stderr budget is never passed.
			if refused < maxSkipLines {
				leaf := sha256.Sum256(id.Chain[0])
				fmt.Fprintf(d.stderr, "%sskipped the identity whose certificate has SHA-256 %x: %v\n", helperErrorPrefix, leaf, err)
			}
			refused++
			continue
		}
		report.Proofs = append(report.Proofs, identityProof{Chain: id.Chain, SignedAt: signedAt, Signature: sig})
	}
	if refused > maxSkipLines {
		fmt.Fprintf(d.stderr, "%s%d more identities skipped\n", helperErrorPrefix, refused-maxSkipLines)
	}
	if refused > 0 && len(report.Proofs) == 0 {
		return fmt.Errorf("device-identity: no identity could be signed (%d refused)", refused)
	}
	return json.NewEncoder(d.stdout).Encode(report)
}

func derWithinBound(chain [][]byte) bool {
	for _, c := range chain {
		if len(c) == 0 || len(c) > devicecert.MaxDER {
			return false
		}
	}
	return true
}
