// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// The edit actions are tools mode's: the control session answers 404 for each, and the session that
// arrived on the tools port serves them. Native darwin test, like server_darwin_test.go's.
func TestDarwinEditActionsAre404OnTheControlSession(t *testing.T) {
	s, _, h := newDarwinServer(t)
	tools := (&Tools{Desktop: s.Desktop, EditDir: filepath.Join(t.TempDir(), "wardyn-edit"), ItemStageDir: t.TempDir(),
		run: func(context.Context, []string) ([]byte, int, error) { return nil, 0, nil }}).ToolsHandler()
	for _, path := range []string{"/edit/admin-terminal", "/edit/trust-cas", "/edit/full-disk-access", "/edit/shut-down"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("control session: POST %s = %d, want 404", path, w.Code)
		}
		w = httptest.NewRecorder()
		tools.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("tools session: POST %s = %d", path, w.Code)
		}
	}
}
