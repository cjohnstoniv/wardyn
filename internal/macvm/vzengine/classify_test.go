// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Code-Hex/vz/v3"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// The framework's VM limit refusal (VZErrorDomain code 6) is wrapped with macvm.ErrVMLimit and keeps the
// framework's error; any other code, domain or error type is returned as it was.
func TestClassifyStart(t *testing.T) {
	limit := &vz.NSError{Domain: "VZErrorDomain", Code: 6}
	got := classifyStart(limit)
	if !errors.Is(got, macvm.ErrVMLimit) {
		t.Fatalf("code 6 did not wrap ErrVMLimit: %v", got)
	}
	var ns *vz.NSError
	if !errors.As(got, &ns) || ns != limit {
		t.Fatalf("the framework's error was lost: %v", got)
	}
	if !errors.Is(classifyStart(fmt.Errorf("start: %w", limit)), macvm.ErrVMLimit) {
		t.Fatal("a wrapped code 6 was not classified")
	}
	for _, err := range []error{&vz.NSError{Domain: "VZErrorDomain", Code: 1}, &vz.NSError{Domain: "NSPOSIXErrorDomain", Code: 6}, errors.New("boom")} {
		if out := classifyStart(err); errors.Is(out, macvm.ErrVMLimit) || out != err {
			t.Fatalf("%v was classified as a VM limit: %v", err, out)
		}
	}
	if int(vz.ErrorVirtualMachineLimitExceeded) != macvm.VZErrorVMLimitExceeded {
		t.Fatalf("vz's code %d is not macvm.VZErrorVMLimitExceeded", vz.ErrorVirtualMachineLimitExceeded)
	}
}
