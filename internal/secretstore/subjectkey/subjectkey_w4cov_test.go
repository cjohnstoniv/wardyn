// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A destroyed generation is a definitive loss that names which generation of
// whose key it was, and is never read as an outage a caller could ride out.
func TestW4CovDataLossNamesTheGenerationAndIsDefinitive(t *testing.T) {
	err := dataLoss(keyID{"alice", PurposeAuditSeal, 3})
	if !errors.Is(err, ErrDataLoss) || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("dataLoss = %v, want ErrDataLoss and not an outage", err)
	}
	for _, want := range []string{"generation 3", `owner="alice"`, `purpose="audit-seal"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("dataLoss = %q, want it to contain %q", err, want)
		}
	}
}
