// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rowLockProbe reports, for every read of an organisation's row, whether the
// row's lock was held when the read was made.
type rowLockProbe struct {
	*authzStore
	srv  *Server
	held []bool
}

func (p *rowLockProbe) GetComponent(ctx context.Context, id uuid.UUID, owner string) (types.Component, error) {
	if owner == "" {
		_, unlock, free := p.srv.tryLock(context.Background(), db.SiteConfigLockClass, cliRowLockPart, id.String())
		if free {
			unlock()
		}
		p.held = append(p.held, !free)
	}
	return p.authzStore.GetComponent(ctx, id, owner)
}

// PUT /components/{id} reads the row it compares the new definition against
// under the lock a CLI sign-in's capture holds, for a create and for an update
// alike. Read before the lock, two writes could race: one would compare against
// a row the other is re-pinning, purge nothing, and leave a sign-in captured
// for a pin the row no longer has.
func TestPutComponent_ReadsTheRowUnderItsLock(t *testing.T) {
	srv, ast, _ := componentsServer(t)
	probe := &rowLockProbe{authzStore: ast, srv: srv}
	srv.cfg.Store = probe
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	id := uuid.NewString()
	for i, step := range []string{"create", "update", "re-pin"} {
		subs := `["` + azCLISubA + `"]`
		if step == "re-pin" {
			subs = `["` + azCLISubB + `"]`
		}
		body := saveComponentBody("Azure", `{"cli":{"kind":"azure","azure":{"subscriptions":`+subs+`,"access":"read"}}}`)
		before := len(probe.held)
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id, admin, body); w.Code >= 300 {
			t.Fatalf("%s: PUT = %d %s", step, w.Code, w.Body.String())
		}
		reads := probe.held[before:]
		if len(reads) == 0 {
			t.Fatalf("%s: the door read no row (step %d)", step, i)
		}
		for _, held := range reads {
			if !held {
				t.Errorf("%s: the row was read outside its lock", step)
			}
		}
	}
	if _, unlock, free := srv.tryLock(context.Background(), db.SiteConfigLockClass, cliRowLockPart, id); !free {
		t.Error("the door kept the row's lock after it answered")
	} else {
		unlock()
	}
}
