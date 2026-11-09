// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func (g *buttonGuest) EditInstallRosetta(context.Context) (guestagent.EditActionResult, error) {
	g.record("install-rosetta")
	return guestagent.EditActionResult{Installed: true}, nil
}

// The Install Rosetta button asks the window boot's guest for the install, only when it is pressed, and says
// what came of it.
func TestEditWindowInstallRosettaButton(t *testing.T) {
	var got []string
	e, g := buttonEdit(t, nil, "", func(text macvm.WindowText, w *liveWin) {
		if !w.waitFor("Read by the guest now") {
			got = append(got, "no first read")
			return
		}
		if text.InstallRosetta == nil {
			got = append(got, "the Install Rosetta button has no callback")
			return
		}
		text.InstallRosetta()
		if want := "Rosetta is installed in the VM."; !w.waitFor(want) {
			got = append(got, fmt.Sprintf("after the click the window shows %q, want %q", w.last(), want))
		}
	})
	if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Error(f)
	}
	if want := []string{"install-rosetta"}; !slices.Equal(g.calls, want) {
		t.Errorf("the guest was asked %q, want %q", g.calls, want)
	}
}

// With FileVault on in the guest the window's live line carries the warning; off or unread it does not.
func TestEditWindowWarnsWhenFileVaultIsOn(t *testing.T) {
	for _, tc := range []struct {
		fv   string
		warn bool
	}{{guestagent.FileVaultOn, true}, {guestagent.FileVaultOff, false}, {"", false}} {
		t.Run("fv="+tc.fv, func(t *testing.T) {
			var got []string
			e, g := buttonEdit(t, nil, "", func(_ macvm.WindowText, w *liveWin) {
				if !w.waitFor("Read by the guest now") {
					got = append(got, "no first read")
					return
				}
				if has := strings.Contains(w.last(), "FileVault"); has != tc.warn {
					got = append(got, fmt.Sprintf("live line %q: FileVault warning present = %v, want %v", w.last(), has, tc.warn))
				}
			})
			g.fv = tc.fv
			if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
				t.Fatal(err)
			}
			for _, f := range got {
				t.Error(f)
			}
		})
	}
}

// The recheck prints the warning once when the guest reads FileVault on, and keeps the edited base.
func TestEditBaseRecheckWarnsWhenFileVaultIsOn(t *testing.T) {
	for _, tc := range []struct {
		fv   string
		want int
	}{{guestagent.FileVaultOn, 1}, {guestagent.FileVaultOff, 0}, {"", 0}} {
		t.Run("fv="+tc.fv, func(t *testing.T) {
			e := newEdit(t)
			e.g.posture.FileVault = tc.fv
			if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(e.out.String(), macvm.FileVaultWarning(guestagent.FileVaultOn)); n != tc.want {
				t.Errorf("the warning printed %d times, want %d:\n%s", n, tc.want, e.out.String())
			}
			if m, err := macvm.ReadyBase(e.base); err != nil || macvm.BaseEditedAt(e.base, m) == nil {
				t.Errorf("the edited base was not kept: %v", err)
			}
		})
	}
}

// Only "on" is a warning.
func TestFileVaultWarning(t *testing.T) {
	for state, warn := range map[string]bool{"on": true, "off": false, "": false, "maybe": false} {
		if got := macvm.FileVaultWarning(state) != ""; got != warn {
			t.Errorf("FileVaultWarning(%q) warns = %v, want %v", state, got, warn)
		}
	}
}

// fileVaultGuest is an item guest whose posture reads FileVault as fv.
type fileVaultGuest struct {
	*itemGuest
	fv string
}

func (g fileVaultGuest) Posture(context.Context) (guestagent.ToolsPosture, error) {
	return guestagent.ToolsPosture{FileVault: g.fv}, nil
}

// The recheck's read-back records FileVault on or off in the base's guest posture, and nothing for an
// answer it does not know.
func TestReadBackItemsRecordsFileVault(t *testing.T) {
	for fv, want := range map[string]string{"on": "on", "off": "off", "": "", "maybe": ""} {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := macvm.ReadBackItems(context.Background(), fileVaultGuest{newItemGuest(), fv}, base); err != nil {
			t.Fatal(err)
		}
		if rec, _ := macvm.ReadToolsRecord(base); rec.Guest == nil || rec.Guest.FileVault != want {
			t.Errorf("posture %q: recorded %+v, want file_vault %q", fv, rec.Guest, want)
		}
	}
}
