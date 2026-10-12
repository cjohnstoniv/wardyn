// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

// The live acceptance case (#588): a real AWS KMS key. WARDYN_TEST_AWS_KMS_KEY
// is the key's full ARN; the identity is whatever the AWS SDK's default
// credential chain finds (IRSA or EKS Pod Identity on EKS, a profile on a
// workstation), allowed kms:Encrypt and kms:Decrypt on that key only.
// WARDYN_TRUSTED_CA_FILE is honoured as wardynd honours it.

import (
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"
)

func TestLive_AWSKMS(t *testing.T) {
	keyARN := os.Getenv("WARDYN_TEST_AWS_KMS_KEY")
	if keyARN == "" {
		t.Skip("WARDYN_TEST_AWS_KMS_KEY not set; skipping the live AWS KMS case")
	}
	kektest.Run(t, func(t *testing.T) kek.KEK {
		k, err := NewKEK(t.Context(), Config{KeyARN: keyARN, Timeout: 30 * time.Second, CACertFile: os.Getenv("WARDYN_TRUSTED_CA_FILE")})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s, long-lived access key: %v", k.Describe(), k.StaticKey())
		return k
	}, kektest.Hooks{})
}
