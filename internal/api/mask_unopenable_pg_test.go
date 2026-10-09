// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's runtime-injected value lives only in the shared registry. Once that
// value can never be opened again (a credential-only erase destroyed the
// owner's key, or its ciphertext is damaged), a replica that never cached it
// refuses the run's recording upload and persists nothing, instead of vouching
// for a corpus that is missing the value.
func TestPG_RecordingUploadRefusedAfterCredentialOnlyErase(t *testing.T) {
	for _, mode := range []string{"credential-only erase", "corrupted ciphertext"} {
		t.Run(mode, func(t *testing.T) {
			l := newMaskLab(t)
			a := l.replica()
			run := l.run()
			a.dispatch(t, run) // a complete, empty manifest
			if err := a.reg.AddGlobal(0, maskOwner, "erased-credential", time.Now(), []byte("global-credential-copy-XYZ789")); err != nil {
				t.Fatal(err)
			}
			var later types.AgentRun
			const value = "runtime-injected-credential-ABC123"
			if err := a.srv.maskInjected(run.ID, []byte(value)); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "credential-only erase":
				rep, err := a.srv.erasureOrchestrator(nil).Orchestrate(context.Background(), maskOwner, []erasure.Scope{erasure.Credentials})
				if err != nil {
					t.Fatal(err)
				}
				if d, _ := rep.Details[erasure.Credentials].(map[string]any); d["runs_fenced"] != 1 {
					t.Errorf("credentials detail = %v, want runs_fenced 1", rep.Details[erasure.Credentials])
				}
				// A run the person starts under the next key generation holds nothing the
				// destroyed key sealed: no replica's read of the retired copy may fence it.
				later = l.run()
				a.dispatch(t, later)
			case "corrupted ciphertext":
				if _, err := l.pool.Exec(t.Context(),
					`UPDATE mask_values SET sealed = set_byte(sealed, octet_length(sealed)-1, get_byte(sealed, octet_length(sealed)-1) # 1) WHERE run_id=$1`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			b := l.replica()
			saved := recording.NewPGStore(l.pool)
			b.srv.cfg.RecordingStore = saved
			wantMaskRefusal(t, l.upload(b, run, "out: "+value+"\n"), "upload to a replica that cannot open the run's value")
			if rc, err := saved.OpenCast(t.Context(), run.ID.String()); err == nil {
				body, _ := io.ReadAll(rc)
				rc.Close()
				if strings.Contains(string(body), value) {
					t.Fatal("the recording was saved with the credential in the clear")
				}
				t.Fatal("a recording was saved for a refused upload")
			}
			if n := l.count(`SELECT count(*) FROM run_mask_manifest WHERE run_id=$1 AND fenced_at IS NOT NULL`, run.ID); n != 1 {
				t.Errorf("the run's manifest is not fenced (%d fenced rows)", n)
			}
			if mode == "credential-only erase" {
				if w := l.upload(b, later, "ok\n"); w.Code != http.StatusNoContent {
					t.Errorf("a run dispatched after the erase: upload status %d, want 204: %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
