// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func folderBringBack(t *testing.T, status int, body string) (string, string, error) { // stdout, stderr, err
	t.Helper()
	id := uuid.New()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	root := rootCmd()
	var out, errOut strings.Builder
	root.SetArgs([]string{"folder", "bring-back", "--url", srv.URL, "--token", "tok", id.String()})
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	if want := "POST /api/v1/runs/" + id.String() + "/computer-folder/bring-back Bearer tok"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	return out.String(), errOut.String(), err
}

func TestFolderBringBack_PrintsWhatCameBack(t *testing.T) {
	out, _, err := folderBringBack(t, http.StatusOK,
		`{"state":"STOPPED","sandbox_removed":true,"back":{"at":"2026-10-22T14:32:00Z","files":12,"bytes":900,"refused":[{"path":"secrets.env","reason":"denied"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"left out: secrets.env (denied)\n", "12 files came back to you.\n", folderCopy.deleted} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestFolderBringBack_SaysWhenTheSandboxWasNotDeleted(t *testing.T) {
	out, errOut, err := folderBringBack(t, http.StatusOK,
		`{"state":"STOPPED","sandbox_removed":false,"back":{"at":"2026-10-22T14:32:00Z","files":3,"refused":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3 files came back to you.\n") || strings.Contains(out, folderCopy.deleted) || errOut != folderCopy.progress+folderCopy.notDeleted {
		t.Fatalf("stdout %q, stderr %q: want the folder's line, and the sandbox not claimed deleted", out, errOut)
	}
}

func TestFolderBringBack_AFailedPassIsAnError(t *testing.T) {
	out, _, err := folderBringBack(t, http.StatusOK, `{"state":"STOPPED","back":{"at":"2026-10-22T14:32:00Z","files":0,"refused":[],"error":"the runner went offline"}}`)
	if err == nil || !strings.Contains(err.Error(), "the runner went offline") || strings.Contains(out, "came back to you") {
		t.Fatalf("err %v, output %q: want the pass's error and no success line", err, out)
	}
}

func TestFolderBringBack_ShowsTheServersRefusal(t *testing.T) {
	_, _, err := folderBringBack(t, http.StatusConflict,
		`{"error":"This run is no longer running, so its folder can't come back. Killing a run doesn't bring the folder back.","reason":"computer_folder_not_running"}`)
	if err == nil || !strings.Contains(err.Error(), "Killing a run doesn't bring the folder back.") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
}

// bringBackAnswer is a success answer with n refusals whose path and reason are 300 characters of & < >,
// encoded as the server encodes it (six bytes each), so 50 of them pass 64 KiB.
func bringBackAnswer(t *testing.T, n int, text string) string {
	t.Helper()
	type refusal struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	refused := make([]refusal, n)
	for i := range refused {
		refused[i] = refusal{text, text}
	}
	b, err := json.Marshal(map[string]any{"state": "STOPPED", "sandbox_removed": true,
		"back": map[string]any{"at": "2026-10-22T14:32:00Z", "files": 7, "bytes": 1, "refused": refused}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The run has stopped and a bring-back runs once, so a valid receipt longer than the 64 KiB an error
// body is cut to is read whole; an answer past the bound is said, not parsed as a cut-off receipt.
func TestFolderBringBack_ReadsAReceiptOver64KiB(t *testing.T) {
	answer := bringBackAnswer(t, 50, strings.Repeat("&<>", 100))
	if len(answer) <= 1<<16 {
		t.Fatalf("answer is %d bytes, want it over 64 KiB", len(answer))
	}
	out, _, err := folderBringBack(t, http.StatusOK, answer)
	if err != nil || strings.Count(out, "left out: ") != 50 || !strings.Contains(out, "7 files came back to you.\n") {
		t.Fatalf("err %v, output of %d bytes with %d left-out lines; want the whole receipt", err, len(out), strings.Count(out, "left out: "))
	}
	_, _, err = folderBringBack(t, http.StatusOK, bringBackAnswer(t, 1, strings.Repeat("<", maxBringBackAnswer/6+1)))
	if err == nil || !strings.Contains(err.Error(), "could not be read") || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want the over-bound answer said", err)
	}
}
