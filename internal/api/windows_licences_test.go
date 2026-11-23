package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /me/windows-images lists an active image only while its pool is active and the caller holds a seat in it.
func TestMyWindowsImagesSkipsADeletedPool(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	ws.holders = []types.WindowsLicenceHolder{{ID: uuid.New(), PoolID: ws.pool.ID, Subject: "sub-ana"}}
	list := func() []string {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me/windows-images", ssoSession(t, "sub-ana", "ana@example.com", oidc.RoleUser), "")
		var out struct {
			Images []types.WindowsImage `json:"images"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("list = %d %s (%v)", w.Code, w.Body, err)
		}
		var names []string
		for _, img := range out.Images {
			names = append(names, img.Name)
		}
		return names
	}
	if got := list(); !slices.Equal(got, []string{"Server"}) {
		t.Errorf("an active pool's images = %v, want [Server]", got)
	}
	ws.pool.State = types.WindowsLicencePoolDeleted
	if got := list(); len(got) != 0 {
		t.Errorf("a deleted pool's images = %v, want none", got)
	}
}

// A malformed name on PUT is a 400 windows_licence_invalid, as on create, before the store is asked.
func TestWindowsLicencePutMalformedName(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	for _, path := range []string{"/api/v1/windows-licence-pools/" + ws.pool.ID.String(), "/api/v1/windows-images/" + ws.images[0].ID.String()} {
		for _, name := range []string{"", " padded", "two\nlines", strings.Repeat("n", types.WindowsLicenceNameMax+1)} {
			body, _ := json.Marshal(map[string]any{"name": name, "revision": 1})
			w := do(t, srv, http.MethodPut, path, adminToken, string(body))
			if w.Code != http.StatusBadRequest || errorReasonOf(w.Body.String()) != reasonWindowsLicenceInvalid {
				t.Errorf("PUT %s name %q = %d %s, want 400 %s", path, name, w.Code, w.Body, reasonWindowsLicenceInvalid)
			}
		}
	}
}

// Each admin family writes its own success row: a pool create, an image register and a holder add.
func TestWindowsLicenceAdminAuditRows(t *testing.T) {
	srv, ws, h := winvmServer(t, true)
	pools := "/api/v1/windows-licence-pools"
	for _, tc := range []struct{ path, body, action, key, want string }{
		{pools, `{"name":"Eng","seats":2}`, auditWindowsPoolCreate, "name", "Eng"},
		{"/api/v1/windows-images", `{"name":"S2","ref":"` + winvmRef + `","licence_pool_id":"` + ws.pool.ID.String() + `"}`, auditWindowsImageRegister, "image", winvmRef},
		{pools + "/" + ws.pool.ID.String() + "/holders", `{"subject":"ana@example.com"}`, auditWindowsHolderAdd, "subject", "ana@example.com"},
	} {
		if w := do(t, srv, http.MethodPost, tc.path, adminToken, tc.body); w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d %s", tc.path, w.Code, w.Body)
		}
		var rows []types.AuditEvent
		for _, ev := range h.audit.snapshot() {
			if ev.Action == tc.action {
				rows = append(rows, ev)
			}
		}
		if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Target == "" {
			t.Fatalf("%s rows = %+v, want one success with a target", tc.action, rows)
		}
		var data map[string]any
		if err := json.Unmarshal(rows[0].Data, &data); err != nil || data[tc.key] != tc.want {
			t.Errorf("%s data = %v (%v), want %s %q", tc.action, data, err, tc.key, tc.want)
		}
	}
}
