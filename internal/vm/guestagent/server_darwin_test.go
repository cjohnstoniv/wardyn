// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Native darwin tests: they run on the train's hosted macOS job, not on Linux.

func newDarwinServer(t *testing.T) (*Server, *fakeRunner, http.Handler) {
	t.Helper()
	r := &fakeRunner{}
	s := &Server{Version: "t", Home: t.TempDir(), User: "agent", OutDir: t.TempDir(), MarkerDir: t.TempDir(),
		Desktop: &Desktop{Run: r, Console: fakeConsole{uid: 502, name: "agent"}, NewPassword: func() (string, error) { return "Abcd2345", nil }}}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, r, h
}

func postInit(h http.Handler, in Init) *httptest.ResponseRecorder {
	b, _ := json.Marshal(in)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/init", bytes.NewReader(b)))
	return w
}

func TestDarwinInitRunDiskDesktopAndCA(t *testing.T) {
	s, r, h := newDarwinServer(t)
	w := postInit(h, Init{Env: map[string]string{"A": "b"}})
	if w.Code != 200 {
		t.Fatalf("init = %d %s", w.Code, w.Body)
	}
	var res InitResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.SignIns != SignInsRunDisk {
		t.Errorf("InitResult = %+v, %v", res, err)
	}
	if pw, ok := s.Desktop.Password(); !ok || pw != "Abcd2345" {
		t.Errorf("no run password after Init: %q %v", pw, ok)
	}
	if len(r.calls) == 0 || r.calls[0][0] != kickstart {
		t.Errorf("Init did not set the VNC password: %v", r.calls)
	}
	if w := postInit(h, Init{}); w.Code != http.StatusConflict {
		t.Errorf("second Init = %d", w.Code)
	}
}

func TestDarwinInitRefusesWhatMacOSCannotDo(t *testing.T) {
	_, _, h := newDarwinServer(t)
	for name, in := range map[string]Init{
		"read-only sign-ins": {SignInsOff: true},
		"folder mount":       {Mounts: []Mount{{Tag: "wardyn0", Target: "/Users/agent/work"}}},
	} {
		if w := postInit(h, in); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: Init = %d", name, w.Code)
		}
	}
	// A refusal leaves the boot's Init available.
	if w := postInit(h, Init{}); w.Code != 200 {
		t.Errorf("Init after refusals = %d", w.Code)
	}
}
