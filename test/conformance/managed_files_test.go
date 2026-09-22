// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The managed-file case must run on the path the product ships, not on a
// benign stand-in: a fixture somewhere nothing can reach passes on a substrate
// that fails the real consumer.
func TestManagedFilesCaseUsesTheConsumerPath(t *testing.T) {
	if managedReadablePath != runner.ManagedFileDir+"/managed-settings.json" {
		t.Errorf("the managed-file case delivers %s, want the consumer's path %s/managed-settings.json", managedReadablePath, runner.ManagedFileDir)
	}
	if err := runner.ValidateManagedFiles(managedFilesFixture()); err != nil {
		t.Errorf("the managed-file case's own fixture is refused by the contract: %v", err)
	}
}
