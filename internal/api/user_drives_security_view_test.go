// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A security admin reads GET /drives through an allow-list projection
// (user_drives_security_view.go). These two sets are the census: every field of
// every type that body is built from is either COPIED for a security admin or
// SUPER-ONLY. A field in neither fails TestSecurityAdminDrivesViewIsAnAllowList
// by name, so a field added to a drive, an allocation or the response cannot
// reach a security admin without someone placing it here.
var (
	securityViewCopied = map[string]bool{
		"userDrivesResponse.Drives":     true,
		"userDrivesResponse.Grants":     true,
		"userDrivesResponse.Disabled":   true,
		"userDrivesResponse.GrantTotal": true,

		"UserDriveListItem.UserDrive":  true,
		"UserDriveListItem.GrantCount": true,

		"UserDrive.ID":           true,
		"UserDrive.Name":         true,
		"UserDrive.Backend":      true,
		"UserDrive.SizeMiB":      true,
		"UserDrive.Writable":     true,
		"UserDrive.Reclaim":      true,
		"UserDrive.HomeTemplate": true,
		"UserDrive.ObjectScheme": true,
		"UserDrive.CreatedAt":    true,
		"UserDrive.UpdatedAt":    true,
		"UserDrive.CreatedBy":    true,

		"UserDriveGrant.ID":               true,
		"UserDriveGrant.SubjectType":      true,
		"UserDriveGrant.Subject":          true,
		"UserDriveGrant.DriveID":          true,
		"UserDriveGrant.Priority":         true,
		"UserDriveGrant.SizeMiBOverride":  true,
		"UserDriveGrant.WritableOverride": true,
		"UserDriveGrant.HomeOverride":     true,
		"UserDriveGrant.Enabled":          true,
		"UserDriveGrant.CreatedAt":        true,
		"UserDriveGrant.CreatedBy":        true,
	}
	securityViewSuperOnly = map[string]bool{
		"userDrivesResponse.HostRootsConfigured": true,
		"userDrivesResponse.RunnerTarget":        true,

		"UserDrive.HostRoot":        true,
		"UserDrive.StorageClass":    true,
		"UserDrive.OwnerSubject":    true,
		"UserDrive.DeleteStartedAt": true,

		// Fields added after the projection was written: securityAdminDrive copies none of them, so a
		// security admin's read goes without them. Copying one is a decision, made in both places.
		"UserDriveListItem.SingleHost":        true,
		"UserDriveListItem.PlacementEligible": true,
		"UserDriveListItem.MountsFrom":        true,
		"UserDrive.Team":                      true,
		"UserDrive.KeptFromRun":               true,
		"UserDrive.AltBackend":                true,
		"UserDrive.AltHostRoot":               true,
		"UserDrive.AltRunnerID":               true,
		"UserDrive.HomeRunnerID":              true,
		"UserDriveGrant.MembersCanWrite":      true,
	}
)

// fillNonZero sets every settable field under v to a non-zero value.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint8:
		v.SetUint(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillNonZero(t, v.Index(i))
		}
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(t, v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillNonZero(t, v.Index(0))
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1, 0)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fillNonZero(t, v.Field(i))
			}
		}
	default:
		t.Fatalf("fillNonZero: unhandled kind %s for %s; extend the census helper", v.Kind(), v.Type())
	}
}

// TestSecurityAdminDrivesViewIsAnAllowList fails on a field in neither set, and
// proves the projection keeps exactly the copied set: every copied field comes
// through non-zero, every super-only field comes through zero.
func TestSecurityAdminDrivesViewIsAnAllowList(t *testing.T) {
	walked := []reflect.Type{
		reflect.TypeOf(userDrivesResponse{}),
		reflect.TypeOf(types.UserDriveListItem{}),
		reflect.TypeOf(types.UserDrive{}),
		reflect.TypeOf(types.UserDriveGrant{}),
	}
	isWalked := func(typ reflect.Type) bool {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		for _, w := range walked {
			if w == typ {
				return true
			}
		}
		return false
	}
	leaf := func(typ reflect.Type) bool {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		return typ.Kind() != reflect.Struct || typ == reflect.TypeOf(time.Time{})
	}
	for _, typ := range walked {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key := typ.Name() + "." + f.Name
			c, s := securityViewCopied[key], securityViewSuperOnly[key]
			switch {
			case c && s:
				t.Errorf("%s is in both the copied and the super-only set", key)
			case !c && !s:
				t.Errorf("%s is in neither set: place it in securityViewCopied or securityViewSuperOnly and, if copied, in securityAdminDrive/securityAdminGrant/securityAdminDrivesView", key)
			}
			if !leaf(f.Type) && !isWalked(f.Type) {
				t.Errorf("%s has nested type %s that the census does not walk: add it to walked", key, f.Type)
			}
		}
	}
	for key := range securityViewCopied {
		if !fieldExists(walked, key) {
			t.Errorf("stale copied entry %s", key)
		}
	}
	for key := range securityViewSuperOnly {
		if !fieldExists(walked, key) {
			t.Errorf("stale super-only entry %s", key)
		}
	}

	var in userDrivesResponse
	fillNonZero(t, reflect.ValueOf(&in).Elem())
	out := securityAdminDrivesView(in)
	check := func(outV reflect.Value) {
		typ := outV.Type()
		for i := 0; i < typ.NumField(); i++ {
			key := typ.Name() + "." + typ.Field(i).Name
			fv := outV.Field(i)
			if securityViewCopied[key] && fv.IsZero() {
				t.Errorf("%s is classed copied but the projection dropped it", key)
			}
			if securityViewSuperOnly[key] && !fv.IsZero() {
				t.Errorf("%s is classed super-only but the projection copied it", key)
			}
		}
	}
	check(reflect.ValueOf(out))
	check(reflect.ValueOf(out.Drives[0]))
	check(reflect.ValueOf(out.Drives[0].UserDrive))
	check(reflect.ValueOf(out.Grants[0]))
}

func fieldExists(walked []reflect.Type, key string) bool {
	for _, typ := range walked {
		for i := 0; i < typ.NumField(); i++ {
			if typ.Name()+"."+typ.Field(i).Name == key {
				return true
			}
		}
	}
	return false
}
