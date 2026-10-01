// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
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
			var logged lockedBuffer
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

// TestWriteServerError_InternalErrorReasonIsPinned covers the generic,
// otherwise-unclassified 500 every OTHER writeServerError call in this
// package falls through to. Asserts the LITERAL wire value, not the Go
// const, so a rename of reasonInternalError without updating docs/sdk.md
// fails here too.
func TestWriteServerError_InternalErrorReasonIsPinned(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	w := httptest.NewRecorder()
	writeServerError(w, r, "get run", errors.New("boom"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "internal_error" {
		t.Errorf("reason = %q, want the literal %q", got, "internal_error")
	}
}

// TestParseIDParam_InvalidIDReasonIsPinned covers parseIDParam's one
// refusal, reused by every {param} path segment in the package. Asserts the
// LITERAL wire value, not the Go const, so a rename of reasonInvalidIDParam
// without updating docs/sdk.md fails here too.
func TestParseIDParam_InvalidIDReasonIsPinned(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/not-a-uuid", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "not-a-uuid")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	if _, ok := parseIDParam(w, r, "id", "run"); ok {
		t.Fatal("parseIDParam accepted a non-UUID")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "invalid_id_param" {
		t.Errorf("reason = %q, want the literal %q", got, "invalid_id_param")
	}
}
