// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A disk drive's delete that the server took without removing its files (deferred while the Mac is offline,
// record only once the Mac is gone) reaches the caller as such: DeleteMyDriveResult returns the server's
// reason and sentence, and DeleteMyDrive returns nil only for the 204 that means the files are gone.
func TestDeleteMyDriveSaysWhetherTheFilesAreGone(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"done", "", "", http.StatusNoContent},
		{"deferred", `{"reason":"member_drive_delete_deferred","message":"Deleting. kofi-mbp is offline, so the disk is removed when it reconnects."}`, "member_drive_delete_deferred", http.StatusAccepted},
		{"record only", `{"reason":"member_drive_record_only","message":"old-mac-mini is no longer registered, so this drive's disk stays on that Mac. Deleting removes only the record."}`, "member_drive_record_only", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/me/drives/"+id.String() {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if tc.body != "" {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := client.New(srv.URL, "tok")

			res, err := c.DeleteMyDriveResult(context.Background(), id)
			if err != nil || res.Reason != tc.reason || (tc.reason != "") != (res.Message != "") {
				t.Fatalf("DeleteMyDriveResult = %+v, %v; want reason %q", res, err, tc.reason)
			}
			err = c.DeleteMyDrive(context.Background(), id)
			var unfinished *client.MyDriveDeleteUnfinishedError
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("DeleteMyDrive = %v; want nil for a finished delete", err)
				}
				return
			}
			if !errors.As(err, &unfinished) || unfinished.Result != res {
				t.Fatalf("DeleteMyDrive = %v; want a *MyDriveDeleteUnfinishedError carrying %+v", err, res)
			}
		})
	}
}
