// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUIApp_IsDesktop(t *testing.T) {
	for _, tc := range []struct {
		app  UIApp
		want bool
	}{
		{UIApp{Name: "novnc"}, true},
		{UIApp{Name: "desk", Kind: "desktop"}, true},
		{UIApp{Name: "novnc", Kind: "desktop"}, true},
		{UIApp{Name: "code"}, false},
		{UIApp{Name: "desk", Kind: "Desktop"}, false},
	} {
		if got := tc.app.IsDesktop(); got != tc.want {
			t.Errorf("%+v IsDesktop = %v, want %v", tc.app, got, tc.want)
		}
	}
}

func TestUIApp_KindJSONOmittedWhenEmpty(t *testing.T) {
	plain, err := json.Marshal(UIApp{Name: "code", Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != `{"name":"code","port":8080}` {
		t.Errorf("an app with no kind marshals to %s, want no kind key", plain)
	}
	want := UIApp{Name: "desk", Port: 5900, Kind: UIAppKindDesktop}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"desk","port":5900,"kind":"desktop"}` {
		t.Errorf("a desktop app marshals to %s", b)
	}
	var got UIApp
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
