// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

import (
	"reflect"
	"testing"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
)

// The window reads vz.VirtualMachine's embedded *pointer by reflection; a vz release that changes its shape
// must fail here, before a window reads the wrong memory.
func TestVZMachinePointerLayout(t *testing.T) {
	if err := checkVMPointerLayout(reflect.TypeOf((*vz.VirtualMachine)(nil)).Elem()); err != nil {
		t.Fatal(err)
	}
	// A VM that was never built has no object: an error, never a crash.
	if p, err := vmPointer(&vz.VirtualMachine{}); err == nil || p != nil {
		t.Fatalf("a zero VM gave %v, %v; want an error", p, err)
	}
	if p, err := vmPointer(nil); err == nil || p != nil {
		t.Fatalf("a nil VM gave %v, %v; want an error", p, err)
	}
}

func TestVMPointerLayoutRefusesOtherShapes(t *testing.T) {
	type inner struct{ _ptr unsafe.Pointer }
	type pointer struct{ _ptr unsafe.Pointer }
	for name, typ := range map[string]reflect.Type{
		"not a struct":    reflect.TypeOf(0),
		"no embedded ptr": reflect.TypeOf(struct{ pointer *inner }{}),
		"wrong package":   reflect.TypeOf(struct{ *pointer }{}),
	} {
		if err := checkVMPointerLayout(typ); err == nil {
			t.Errorf("%s: layout accepted", name)
		}
	}
}

// An invalid reflect Value is an error, never a panic.
func TestVMPointerInvalidValueIsAnError(t *testing.T) {
	if p, err := readVMPointer(reflect.Value{}); err == nil || p != nil {
		t.Fatalf("an invalid Value gave %v, %v; want an error", p, err)
	}
	if p, err := readVMPointer(reflect.ValueOf(struct{ x int }{})); err == nil || p != nil {
		t.Fatalf("another shape gave %v, %v; want an error", p, err)
	}
}
