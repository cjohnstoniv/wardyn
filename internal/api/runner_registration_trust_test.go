// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunnerCannotReportItsTrust: nothing a runner sends (its registration, its caps event and the
// posture inside it) has a field that could carry a trust class or an owner kind, so none can feed them.
func TestRunnerCannotReportItsTrust(t *testing.T) {
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			if strings.Contains(name, "trust") || strings.Contains(name, "owner") {
				t.Errorf("%s.%s (json %q) could carry a runner's trust class or owner kind", path, f.Name, f.Tag.Get("json"))
			}
			walk(path+"."+f.Name, f.Type)
		}
	}
	walk("RunnerRegisterRequest", reflect.TypeOf(types.RunnerRegisterRequest{}))
	walk("runnerwire.Caps", reflect.TypeOf(runnerwire.Caps{}))
	walk("RunnerPosture", reflect.TypeOf(types.RunnerPosture{}))
}

// TestRunnerRegisterRefusesATrustClass: the registration body is strict-decoded, so a runner naming a
// trust class or an owner kind is refused before its token is spent.
func TestRunnerRegisterRefusesATrustClass(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	f.tokens["wdr_raw"] = types.RunnerRegistrationToken{Owner: "alice"}
	for _, field := range []string{`"trust_class":"org_cluster"`, `"owner_kind":"org"`} {
		body := `{"token":"wdr_raw","public_key":"` + strings.Repeat("A", 43) + `=","name":"laptop",` + field + `}`
		if w := do(t, srv, http.MethodPost, "/api/v1/runners/register", "", body); w.Code != http.StatusBadRequest {
			t.Fatalf("register with %s = %d %s, want 400", field, w.Code, w.Body)
		}
	}
	if f.tokens["wdr_raw"].ConsumedAt != nil {
		t.Fatal("a refused registration spent its token")
	}
}
