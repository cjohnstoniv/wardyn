// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestFederatedAuditEvent_MarshalHasNoSalt: no field of AuditEvent or FederatedAuditEvent carries the hash
// format or either salt on the wire; only the federation-only FederatedAuditRow does.
func TestFederatedAuditEvent_MarshalHasNoSalt(t *testing.T) {
	banned := map[string]bool{"hash_salt": true, "claim_salt": true, "hash_format": true}
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if banned[name] {
				t.Errorf("%s.%s is %q on the wire", path, f.Name, name)
			}
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(AuditEvent{}), "AuditEvent")
	walk(reflect.TypeOf(FederatedAuditEvent{}), "FederatedAuditEvent")

	row := reflect.TypeOf(FederatedAuditRow{})
	for _, want := range []string{"HashFormat", "HashSalt"} {
		if _, ok := row.FieldByName(want); !ok {
			t.Errorf("FederatedAuditRow has no %s", want)
		}
	}
}
