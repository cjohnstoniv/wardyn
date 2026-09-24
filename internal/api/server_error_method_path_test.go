// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCeilingAndDriveErrorsLogMethodAndPath (#189) pins the fix for
// writeCeilingError, writeCeilingErrorPrefixed and writeDriveError's 500 arms:
// they used to log through loggedMsg(context.Background(), ...), so the line
// an operator read carried no method, no path and no trace id. All three now
// route through writeServerError(w, r, ...), which logs both.
func TestCeilingAndDriveErrorsLogMethodAndPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"writeCeilingError", "/api/v1/policies/default", func(w http.ResponseWriter, r *http.Request) {
			writeCeilingError(w, r, errors.New("resolve boom"))
		}},
		{"writeCeilingErrorPrefixed", "/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
			writeCeilingErrorPrefixed(w, r, "list secrets: ", errors.New("resolve boom"))
		}},
		{"writeDriveError", "/api/v1/me", func(w http.ResponseWriter, r *http.Request) {
			writeDriveError(w, r, errors.New("resolve boom"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			tc.call(w, r)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
			}
			line := logged.String()
			if !strings.Contains(line, "method=GET") {
				t.Errorf("%s: logged line carries no method: %s", tc.name, line)
			}
			if !strings.Contains(line, "path="+tc.path) {
				t.Errorf("%s: logged line carries no path: %s", tc.name, line)
			}
		})
	}
}
