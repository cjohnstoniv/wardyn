// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// askRun has the person start a run from the console (its policy is what the step needs) and
// returns the run id they paste.
func askRun(t *testing.T, what string) string {
	t.Helper()
	id := ask(t, "From the console, start a macOS VM run "+what+". Paste its run id:")
	if v := wait(t, id, 5*time.Minute); v.State != "RUNNING" {
		t.Fatalf("run %s is %s, not RUNNING: %s", id, v.State, v.FailureHint)
	}
	return id
}

func askNumber(t *testing.T, q string) float64 {
	t.Helper()
	for {
		if f, err := strconv.ParseFloat(ask(t, q), 64); err == nil {
			return f
		}
	}
}

// TestP09_Desktop: the guest's desktop through the gateway, early in the window. The
// person drives noVNC; the suite reads the guest and records each answer.
func TestP09_Desktop(t *testing.T) {
	live(t)
	needPerson(t, "the desktop slot, when prepare reports ready and before the automated suite")
	id := askRun(t, "under a recording policy with paste-in ALLOWED and copy-out BLOCKED")
	t.Run("display", func(t *testing.T) {
		out := mustGuest(t, id, "system_profiler SPDisplaysDataType")
		keep(t, "displays.txt", []byte(out))
		if !strings.Contains(out, "1920 x 1080") || strings.Contains(out, "Retina") {
			t.Errorf("the guest does not report 1920 x 1080 at 1x")
		}
		keep(t, "screensharing.txt", []byte(guest(t, id, "sudo -n launchctl print system/com.apple.screensharing | head -30", time.Minute).out))
	})
	t.Run("first_frame", func(t *testing.T) {
		doStep(t, fmt.Sprintf("Open run %s's desktop from the console (over the loopback forward).", id))
		if s := askNumber(t, "Seconds from the click to the first frame (from the gateway's ui.open to the first update in the recorder):"); s > 5 {
			t.Errorf("first frame after %.1fs, budget 5s", s)
		}
		record(t, "create_to_first_frame_s", ask(t, "Seconds from create to the first desktop frame, including any retries:"))
		record(t, "fps", ask(t, "Frames per second while scrolling a page (recorder):"))
		record(t, "input_latency_ms", ask(t, "Input latency added over the network RTT, in ms:"))
		record(t, "encodings", ask(t, "Encodings the shim negotiated (shim log):"))
	})
	t.Run("control", func(t *testing.T) {
		m := marker()
		doStep(t, "In the guest, open TextEdit and type exactly "+m+" through noVNC.")
		r := guest(t, id, `osascript -e 'tell application "TextEdit" to get text of document 1'`, time.Minute)
		if !strings.Contains(r.out, m) && !confirm(t, "The guest could not be read; does TextEdit in the guest show "+m+"?") {
			t.Error("keys typed in noVNC did not reach the guest")
		}
		p0 := guest(t, id, `osascript -l JavaScript -e 'ObjC.import("AppKit"); JSON.stringify($.NSEvent.mouseLocation)'`, time.Minute).out
		doStep(t, "Move the pointer across the guest desktop through noVNC.")
		p1 := guest(t, id, `osascript -l JavaScript -e 'ObjC.import("AppKit"); JSON.stringify($.NSEvent.mouseLocation)'`, time.Minute).out
		record(t, "pointer_before", strings.TrimSpace(p0))
		record(t, "pointer_after", strings.TrimSpace(p1))
		if p0 == p1 && !confirm(t, "The pointer reading did not change; did the guest's pointer move?") {
			t.Error("a pointer move in noVNC did not move the guest's pointer")
		}
	})
	t.Run("clipboard", func(t *testing.T) {
		in := marker()
		doStep(t, "Copy "+in+" on your side and paste it into the guest through the viewer.")
		if !strings.Contains(guest(t, id, "pbpaste", time.Minute).out, in) {
			t.Error("paste-in is allowed but the marker did not reach the guest's clipboard")
		}
		crossed := 0
		for i := range 10 {
			out := marker()
			mustGuest(t, id, "printf %s "+out+" | pbcopy")
			got := ask(t, fmt.Sprintf("Try %d/10: paste the viewer's clipboard here (or type empty):", i+1))
			if strings.Contains(got, out) {
				crossed++
			}
		}
		record(t, "copy_out_crossed", crossed)
		if crossed > 0 {
			t.Errorf("copy-out is blocked but a guest marker reached the viewer %d of 10 times", crossed)
		}
		record(t, "copy_out_proof", ask(t, "Which shows the blocked direction: 'audit' (the gateway audit's blocked transfer) or 'no channel' (the shim log shows no standard cut text in 10 tries)?"))
		if !confirm(t, "Did any Apple message type the shim does not know reach the viewer without a log line? (yes means a failure)") {
			return
		}
		t.Error("an unknown Apple message type crossed without a log line")
	})
	t.Run("paste_in_blocked", func(t *testing.T) {
		id2 := askRun(t, "with paste-in BLOCKED")
		m := marker()
		doStep(t, fmt.Sprintf("Open run %s's desktop, copy %s on your side and paste it into the guest.", id2, m))
		if strings.Contains(guest(t, id2, "pbpaste", time.Minute).out, m) {
			t.Error("paste-in is blocked but the marker reached the guest")
		}
	})
	t.Run("recording", func(t *testing.T) {
		if !confirm(t, fmt.Sprintf("End run %s's desktop, open its recording: does it play the session back?", id)) {
			t.Error("the recording does not play the session back")
		}
	})
}

// TestP09b_LocalWindow: only if the local-window desktop was chosen.
func TestP09b_LocalWindow(t *testing.T) {
	live(t)
	if !cfg.localWindow {
		t.Skip("the local-window desktop was not chosen (WARDYN_MACVM_LOCAL_WINDOW is not 1)")
	}
	needPerson(t, "after P9, at the Mac's own screen")
	id := askRun(t, "and open its local window on the Mac's own screen")
	if !confirm(t, "Controls: does copy and paste work within the host, and within the guest?") {
		t.Fatal("a clipboard control failed; the probes would prove nothing")
	}
	for _, dir := range []struct{ name, from, to string }{{"guest_to_host", "guest", "host"}, {"host_to_guest", "host", "guest"}} {
		t.Run(dir.name, func(t *testing.T) {
			crossed := 0
			for i := range 10 {
				m := marker()
				if dir.from == "guest" {
					mustGuest(t, id, "printf %s "+m+" | pbcopy")
					if got := ask(t, fmt.Sprintf("Try %d/10: paste the host clipboard here:", i+1)); strings.Contains(got, m) {
						crossed++
					}
				} else {
					doStep(t, fmt.Sprintf("Try %d/10: copy %s on the host, then paste it in the local window's guest.", i+1, m))
					if strings.Contains(guest(t, id, "pbpaste", time.Minute).out, m) {
						crossed++
					}
				}
			}
			t.Logf("RECORD %s crossed=%d/10", dir.name, crossed)
		})
	}
	for _, step := range []string{"under a short session bound, wait for the bound", "pause the run", "stop the run"} {
		if !confirm(t, "Open the local window again, then "+step+": does the window close, with ui.open and ui.close rows showing via: local_window?") {
			t.Errorf("the local window did not close when asked to %s", step)
		}
	}
}

// TestP10_CleanupBoundary: synthetic markers, a mid-run host snapshot, then what is left.
func TestP10_CleanupBoundary(t *testing.T) {
	live(t)
	m := marker()
	id := start(t, nil)
	dir := runDir(t, id)
	mustGuest(t, id, "printf %s "+m+" > ~/p10-marker.txt")
	switch {
	case cfg.cookieURL != "" && cfg.prompts:
		doStep(t, fmt.Sprintf("In run %s's guest, open Safari at %s?m=%s (it sets a cookie with the marker).", id, cfg.cookieURL, m))
	default:
		t.Log("RECORD safari_cookie=not run (needs WARDYN_MACVM_COOKIE_URL and a person)")
	}
	snap := sh(t, 5*time.Minute, "tmutil", "localsnapshot")
	record(t, "mid_run_snapshot", strings.TrimSpace(snap.out+snap.err))
	keep(t, "snapshots.txt", []byte(sh(t, time.Minute, "tmutil", "listlocalsnapshots", "/").out))
	end(t, id)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("run dir %s still exists after the run ended (%v)", dir, err)
	}
	if out := strings.TrimSpace(sh(t, 2*time.Minute, "mdfind", m).out); out != "" {
		t.Errorf("Spotlight finds the marker: %s", out)
	}
	if ex := sh(t, time.Minute, "tmutil", "isexcluded", layout(t).Root).out; !strings.Contains(ex, "[Excluded]") {
		t.Errorf("tmutil does not report %s excluded: %s", layout(t).Root, ex)
	}
	if agent := os.Getenv("WARDYN_MACVM_NO_SIGNIN_AGENT"); agent != "" {
		if _, err := create(t, map[string]any{"agent": agent}); err == nil || !strings.Contains(err.Error(), "sign-in") {
			t.Errorf("an agent that does not allow sign-ins was not refused a macOS desktop: %v", err)
		}
	} else {
		t.Log("RECORD not_allowed_agent=not run (set WARDYN_MACVM_NO_SIGNIN_AGENT)")
	}
	if cfg.prompts {
		record(t, "marker_in_mid_run_snapshot", ask(t, "Mount the mid-run snapshot read-only and search it for "+m+": found or not found?"))
		record(t, "marker_in_other_snapshots", ask(t, "Search any other listed snapshot for it: found, not found, or none listed?"))
	} else {
		t.Log("RECORD snapshot_search=not run (a person mounts the snapshot; WARDYN_MACVM_PROMPTS=1)")
	}
}

// TestP11_CAAndTools: the works/fails matrix under a door that uses MITM. The matrix is the
// result; the release note must match it.
func TestP11_CAAndTools(t *testing.T) {
	live(t)
	doc := os.Getenv("WARDYN_MACVM_MITM_RUN_DOC")
	need(t, doc, "WARDYN_MACVM_MITM_RUN_DOC (a kind: Run document whose policy's door uses MITM)")
	need(t, cfg.allowedURL, "WARDYN_MACVM_ALLOWED_URL")
	saved := cfg.runDoc
	cfg.runDoc = doc
	id := start(t, nil)
	cfg.runDoc = saved
	u := cfg.allowedURL
	matrix := map[string]string{}
	for name, script := range map[string]string{
		"node":    `node -e 'require("https").get(process.argv[1], r => { console.log(r.statusCode); process.exit(0) }).on("error", e => { console.error(e.message); process.exit(1) })' '` + u + `'`,
		"git":     "git ls-remote '" + env("WARDYN_MACVM_GIT_URL", u) + "' | head -1",
		"curl":    "curl -sS -o /dev/null -w '%{http_code}' '" + u + "'",
		"python3": `python3 -c 'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1]).status)' '` + u + `'`,
	} {
		r := guest(t, id, script, 2*time.Minute)
		matrix[name] = map[bool]string{true: "works", false: "fails: " + strings.TrimSpace(r.err)}[r.code == 0]
	}
	if cfg.prompts {
		matrix["safari"] = ask(t, fmt.Sprintf("In run %s's guest, open %s in Safari: works or fails (with the error)?", id, u))
	} else {
		matrix["safari"] = "not run (a person; WARDYN_MACVM_PROMPTS=1)"
	}
	b, _ := json.MarshalIndent(matrix, "", "  ")
	keep(t, "matrix.json", b)
	t.Logf("RECORD matrix=%s", b)
}
