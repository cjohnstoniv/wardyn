// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// TestImportSpec_RefusesProxyConfig: an import spec that sets any ProxyConfig field, each tried alone, is
// refused before anything is created; the agent half alone passes. A field added to ProxyConfig is covered
// without a change here.
func TestImportSpec_RefusesProxyConfig(t *testing.T) {
	agent := SandboxSpec{RunID: uuid.New(), Image: "agent:1", Env: map[string]string{"A": "1"}, Interactive: true}
	if err := CheckImportSpec(agent); err != nil {
		t.Fatalf("the agent half alone = %v; want nil", err)
	}
	typ := reflect.TypeFor[ProxyConfig]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		spec := agent
		v := reflect.ValueOf(&spec.ProxyConfig).Elem().Field(i)
		if !v.CanSet() {
			t.Fatalf("ProxyConfig.%s cannot be set by the test", f.Name)
		}
		v.Set(nonZero(t, f.Type))
		if err := CheckImportSpec(spec); !errors.Is(err, errImportCarriesPolicy) {
			t.Errorf("a spec setting only ProxyConfig.%s = %v; want errImportCarriesPolicy", f.Name, err)
		}
	}
}

// nonZero is a value of typ that reflect reads as non-zero.
func nonZero(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(typ, 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(typ))
	case reflect.Pointer:
		v.Set(reflect.New(typ.Elem()))
	case reflect.Array:
		v.Index(0).Set(nonZero(t, typ.Elem()))
	case reflect.Struct:
		for i := range typ.NumField() {
			if v.Field(i).CanSet() {
				v.Field(i).Set(nonZero(t, typ.Field(i).Type))
				return v
			}
		}
		t.Fatalf("struct %s has no field the test can set", typ)
	default:
		t.Fatalf("no non-zero value for kind %s (%s)", typ.Kind(), typ)
	}
	return v
}
