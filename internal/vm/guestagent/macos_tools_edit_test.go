// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// admin_code changes the setup admin's password through passwd only (never an argv), takes letters and digits
// only, old and new both, so neither can end dscl's command line, and prints redacted.
func TestToolsAdminCode(t *testing.T) {
	rig := newToolsRig(t)
	var set [][3]string
	rig.t.passwd = func(_ context.Context, user, old, pw string) error {
		set = append(set, [3]string{user, old, pw})
		return nil
	}
	if w := rig.do("POST", "/v1/tools/admin_code", `{"old":"Abcd2345","code":"Efgh6789"}`); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"admin_code_set":true}` {
		t.Fatalf("admin_code = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(set, [][3]string{{MacAdminUser, "Abcd2345", "Efgh6789"}}) {
		t.Fatalf("passwd calls = %v", set)
	}
	for _, bad := range []string{`{"old":"Abcd2345","code":"Abcd2345\nquit"}`, `{"old":"Abcd2345","code":"Abcd 2345"}`, `{"old":"Abcd2345","code":"Abc"}`, `{"old":"Abcd2345"}`,
		`{"old":"Abcd 2345","code":"Efgh6789"}`, `{"old":"Abcd2345\nquit","code":"Efgh6789"}`, `{"old":"Abc","code":"Efgh6789"}`, `{"code":"Efgh6789"}`} {
		if w := rig.do("POST", "/v1/tools/admin_code", bad); w.Code != 400 {
			t.Errorf("admin_code %s = %d, want 400", bad, w.Code)
		}
	}
	if len(set) != 1 || len(rig.ran) != 0 {
		t.Fatalf("a refused password reached passwd (%v) or an argv ran (%v)", set, rig.ran)
	}
	rig.t.passwd = func(context.Context, string, string, string) error { return errors.New("dscl failed") }
	if w := rig.do("POST", "/v1/tools/admin_code", `{"old":"Abcd2345","code":"Efgh6789"}`); strings.TrimSpace(w.Body.String()) != `{"admin_code_set":false}` {
		t.Errorf("a failed passwd = %s", w.Body)
	}
	v := AdminCode{Old: "Abcd2345", Code: "Efgh6789"}
	if s := fmt.Sprintf("%v %+v %#v %s", v, v, v, v); strings.Contains(s, "Abcd2345") || strings.Contains(s, "Efgh6789") {
		t.Errorf("AdminCode prints a password: %s", s)
	}
}

// posture lists dscl's accounts with their uids and says whether anything answers on Remote Login's port.
func TestToolsPosture(t *testing.T) {
	rig := newToolsRig(t)
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		return []byte("_amavisd     83\nagent        502\nnobody       -2\nroot         0\nwardyn-admin 501\n"), 0, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rig.t.sshAddr = l.Addr().String()
	read := func() ToolsPosture {
		t.Helper()
		w := rig.do("GET", "/v1/tools/posture", "")
		var p ToolsPosture
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("posture = %d %s", w.Code, w.Body)
		}
		return p
	}
	p := read()
	want := []GuestAccount{{"_amavisd", 83}, {"agent", 502}, {"nobody", -2}, {"root", 0}, {"wardyn-admin", 501}}
	if !slices.Equal(p.Accounts, want) || !p.RemoteLogin {
		t.Fatalf("posture = %+v", p)
	}
	if !slices.Equal(rig.ran[0], []string{"/usr/bin/dscl", ".", "-list", "/Users", "UniqueID"}) {
		t.Errorf("ran %v", rig.ran[0])
	}
	_ = l.Close()
	if p := read(); p.RemoteLogin {
		t.Error("Remote Login reads on with nothing listening")
	}
	rig.t.run = func(context.Context, []string) ([]byte, int, error) { return []byte("agent\n"), 0, nil }
	if w := rig.do("GET", "/v1/tools/posture", ""); w.Code != 500 {
		t.Errorf("an unreadable account list = %d, want 500", w.Code)
	}
}

// posture says whether the agent may open macOS's protected TCC database: allowed on an open, refused on
// the kernel's EPERM or EACCES, and empty (unknown) on anything else, so a path that is not there is never
// read as allowed. The probe opens the one path and nothing else, and an agent without the field decodes
// as unknown.
func TestToolsPostureFullDiskAccess(t *testing.T) {
	rig := newToolsRig(t)
	rig.t.run = func(context.Context, []string) ([]byte, int, error) { return []byte("agent 502\nroot 0\n"), 0, nil }
	rig.t.sshAddr = "127.0.0.1:1"
	var opened []string
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "allowed"}, {unix.EPERM, "refused"}, {unix.EACCES, "refused"},
		{fmt.Errorf("open: %w", unix.EPERM), "refused"}, {unix.ENOENT, ""}, {errors.New("io"), ""},
	} {
		rig.t.fdaOpen = func(path string) error { opened = append(opened, path); return tc.err }
		w := rig.do("GET", "/v1/tools/posture", "")
		var p ToolsPosture
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.FullDiskAccess != tc.want {
			t.Errorf("open error %v: posture = %d %s, want %q", tc.err, w.Code, w.Body, tc.want)
		}
	}
	if want := "/Library/Application Support/com.apple.TCC/TCC.db"; len(opened) != 6 || len(slices.Compact(slices.Clone(opened))) != 1 || opened[0] != want {
		t.Errorf("the probe opened %q", opened)
	}
	var old ToolsPosture
	if err := json.Unmarshal([]byte(`{"accounts":[],"remote_login":false}`), &old); err != nil || old.FullDiskAccess != "" {
		t.Errorf("an older agent's posture = %+v %v", old, err)
	}
	if b, _ := json.Marshal(ToolsPosture{}); strings.Contains(string(b), "full_disk_access") {
		t.Errorf("an unread field is sent: %s", b)
	}
}
