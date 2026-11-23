// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import "time"

// DeviceCertReport is the device_cert event's data: the device identities the Mac's MDM installed, each
// proving it holds the key by signing devicecert.Message.
type DeviceCertReport struct {
	Proofs []DeviceCertProof `json:"proofs"`
}

// DeviceCertProof is one identity: its certificate chain, when the device key signed, and the signature.
type DeviceCertProof struct {
	Chain     [][]byte  `json:"chain"` // leaf first, DER
	SignedAt  time.Time `json:"signed_at"`
	Signature []byte    `json:"signature"` // ECDSA ASN.1 or RSA PKCS#1 v1.5, over SHA-256 of the message
}
