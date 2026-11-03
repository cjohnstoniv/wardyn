// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"github.com/Code-Hex/vz/v3"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// vz v3.8.0 keeps a VirtualMachine's VZVirtualMachine in an internal package: the struct embeds *pointer
// (an alias of internal/objc.Pointer), whose only field _ptr is the Objective-C object. A run's window draws
// a VZVirtualMachineView on that object, so this file reads it by reflection. It is pinned to v3.8.0 (go.mod):
// checkVMPointerLayout refuses any other shape, TestVZMachinePointerLayout fails the hosted macOS job first,
// and a refusal here only means the run has no window.

var _ macvm.ViewMachine = (*machine)(nil)

// ViewHandle is the run VM's VZVirtualMachine pointer, for macvm.ViewMachine.
func (m *machine) ViewHandle() (unsafe.Pointer, error) { return vmPointer(m.vm) }

// vmPointer reads vm's VZVirtualMachine pointer. It never panics: a shape it does not know is an error.
func vmPointer(vm *vz.VirtualMachine) (unsafe.Pointer, error) {
	if vm == nil {
		return nil, errors.New("vzengine: no VM")
	}
	return readVMPointer(reflect.ValueOf(vm).Elem())
}

// readVMPointer reads the pointer from rv, a vz.VirtualMachine value. An invalid Value anywhere on the way is
// an error: it never calls Elem or Field on one.
func readVMPointer(rv reflect.Value) (p unsafe.Pointer, err error) {
	defer func() {
		if r := recover(); r != nil {
			p, err = nil, fmt.Errorf("vzengine: reading the VM pointer: %v", r)
		}
	}()
	noObject := errors.New("vzengine: the VM has no Objective-C object")
	if !rv.IsValid() {
		return nil, noObject
	}
	if err := checkVMPointerLayout(rv.Type()); err != nil {
		return nil, err
	}
	ptr := rv.FieldByName(vmPointerField)
	if !ptr.IsValid() || ptr.IsNil() {
		return nil, noObject
	}
	obj := ptr.Elem()
	if !obj.IsValid() {
		return nil, noObject
	}
	raw := obj.Field(0)
	if !raw.IsValid() || raw.IsNil() {
		return nil, noObject
	}
	return raw.UnsafePointer(), nil
}

const (
	vmPointerField = "pointer" // the embedded *pointer
	vmObjcField    = "_ptr"    // internal/objc.Pointer's only field
	vmObjcPkg      = "github.com/Code-Hex/vz/v3/internal/objc"
)

// checkVMPointerLayout is nil when t is a struct with an embedded *objc.Pointer called pointer, whose struct
// is one unsafe.Pointer field called _ptr: the shape of vz v3.8.0's VirtualMachine.
func checkVMPointerLayout(t reflect.Type) error {
	bad := func(why string) error {
		return fmt.Errorf("vzengine: vz's VirtualMachine layout is not v3.8.0's (%s), so a run has no window", why)
	}
	if t.Kind() != reflect.Struct {
		return bad("not a struct")
	}
	f, ok := t.FieldByName(vmPointerField)
	if !ok || !f.Anonymous || f.Type.Kind() != reflect.Pointer {
		return bad("no embedded pointer")
	}
	e := f.Type.Elem()
	if e.Kind() != reflect.Struct || !strings.HasSuffix(e.PkgPath(), vmObjcPkg) || e.Name() != "Pointer" {
		return bad("pointer is not internal/objc.Pointer")
	}
	if e.NumField() != 1 || e.Field(0).Name != vmObjcField || e.Field(0).Type.Kind() != reflect.UnsafePointer {
		return bad("objc.Pointer is not one unsafe.Pointer")
	}
	return nil
}
