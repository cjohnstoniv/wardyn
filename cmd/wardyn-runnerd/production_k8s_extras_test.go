// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/keystore"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A k8s runner's caps carry its last failure.
func TestK8sExtrasCarryTheLastFailure(t *testing.T) {
	w := newStatusWriter(t.TempDir())
	w.linkChanged(false, "x", runnerwire.StageTLS)
	extras := k8sExtras(serveOptions{status: w}, &keystore.K8s{}, nil, &spoolSink{})
	if extras.lastFailure == nil {
		t.Fatal("k8s extras report no last failure")
	}
	if f := extras.lastFailure(); f == nil || f.Stage != runnerwire.StageTLS {
		t.Errorf("k8s extras report %+v, want the tls failure", f)
	}
}
