// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// askRun has the person start a run from the console on the window runner (its policy is what the
// step needs) and returns the run id they paste.
func askRun(t *testing.T, what string) string {
	t.Helper()
	id := ask(t, "From the console, start a macOS VM run on runner "+cfg.runner+" "+what+". Paste its run id:")
	v := wait(t, id, 5*time.Minute)
	if v.State != "RUNNING" {
		t.Fatalf("run %s is %s, not RUNNING: %s", id, v.State, v.FailureHint)
	}
	pinned(t, v)
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

var (
	displayRes = regexp.MustCompile(`(?m)^\s*Resolution:\s*(\d+) x (\d+)`)
	displayUI  = regexp.MustCompile(`(?m)^\s*UI Looks like:\s*(\d+) x (\d+)`)
)

// TestP09_Desktop: the guest's desktop through the gateway, early in the window. The person drives
// noVNC; the suite reads the guest and asserts every answer the pass depends on.
func TestP09_Desktop(t *testing.T) {
	live(t)
	block(t)
	needPerson(t, "the desktop slot, when prepare reports ready and before the automated suite")
	id := askRun(t, "under a recording policy with paste-in ALLOWED and copy-out BLOCKED")
	t.Run("display", func(t *testing.T) {
		out := mustGuest(t, id, "system_profiler SPDisplaysDataType")
		keep(t, "displays.txt", []byte(out))
		res, ui := displayRes.FindStringSubmatch(out), displayUI.FindStringSubmatch(out)
		if res == nil || ui == nil {
			t.Fatalf("no Resolution or UI Looks like line in system_profiler")
		}
		// 1x: the backing resolution is the point size, both 1920 x 1080.
		if res[1] != "1920" || res[2] != "1080" || ui[1] != "1920" || ui[2] != "1080" {
			t.Errorf("the guest reports %sx%s backing at %sx%s points, want 1920x1080 at 1x", res[1], res[2], ui[1], ui[2])
		}
		keep(t, "screensharing.txt", []byte(guest(t, id, "launchctl print system/com.apple.screensharing | head -30", time.Minute).out))
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
		pointer := `osascript -l JavaScript -e 'ObjC.import("AppKit"); JSON.stringify($.NSEvent.mouseLocation)'`
		p0 := guest(t, id, pointer, time.Minute).out
		doStep(t, "Move the pointer across the guest desktop through noVNC.")
		p1 := guest(t, id, pointer, time.Minute).out
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
			if got := ask(t, fmt.Sprintf("Try %d/10: paste the viewer's clipboard here (or type empty):", i+1)); strings.Contains(got, out) {
				crossed++
			}
		}
		record(t, "copy_out_crossed", crossed)
		if crossed > 0 {
			t.Errorf("copy-out is blocked but a guest marker reached the viewer %d of 10 times", crossed)
		}
		proof := strings.ToLower(ask(t, "Which shows the blocked direction: 'audit' (the gateway audit's blocked transfer) or 'no channel' (the shim log shows no standard cut text in 10 tries)?"))
		record(t, "copy_out_proof", proof)
		if proof != "audit" && proof != "no channel" {
			t.Errorf("the blocked direction is shown by neither the gateway audit nor the shim log (%q)", proof)
		}
		if confirm(t, "Did any Apple message type the shim does not know reach the viewer without a log line? (yes means a failure)") {
			t.Error("an unknown Apple message type crossed without a log line")
		}
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

// TestP09b_LocalWindow: only if the local-window desktop was chosen. The clipboard rows are
// recorded (the result sets the window's admission rule); the controls, bound and rechecks block.
func TestP09b_LocalWindow(t *testing.T) {
	live(t)
	if !cfg.localWindow {
		notRun(t, "the local-window desktop was not chosen (WARDYN_MACVM_LOCAL_WINDOW is not 1)")
	}
	needPerson(t, "after P9, at the Mac's own screen")
	id := askRun(t, "and open its local window on the Mac's own screen")
	t.Run("controls", func(t *testing.T) {
		block(t)
		if !confirm(t, "Controls: does copy and paste work within the host, and within the guest?") {
			t.Fatal("a clipboard control failed; the probes would prove nothing")
		}
	})
	for _, dir := range []string{"guest_to_host", "host_to_guest"} {
		t.Run(dir, func(t *testing.T) {
			crossed := 0
			for i := range 10 {
				m := marker()
				if dir == "guest_to_host" {
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
			t.Logf("RECORD %s crossed=%d/10", dir, crossed)
		})
	}
	t.Run("bound_and_rechecks", func(t *testing.T) {
		block(t)
		for _, step := range []string{"under a short session bound, wait for the bound", "pause the run", "stop the run"} {
			if !confirm(t, "Open the local window again, then "+step+": does the window close, with ui.open and ui.close rows showing via: local_window?") {
				t.Errorf("the local window did not close when asked to %s", step)
			}
		}
	})
}

// TestP10_CleanupBoundary: synthetic markers, a mid-run host snapshot, then what is left. The
// boundary items block; what the snapshot still holds is recorded.
func TestP10_CleanupBoundary(t *testing.T) {
	live(t)
	m := marker()
	var date string
	t.Run("boundary", func(t *testing.T) {
		block(t)
		id := start(t, spec{})
		dir := runDir(t, id)
		mustGuest(t, id, "printf %s "+m+" > ~/p10-marker.txt")
		if cfg.cookieURL != "" && cfg.prompts {
			doStep(t, fmt.Sprintf("In run %s's guest, open Safari at %s?m=%s (it sets a cookie with the marker).", id, cfg.cookieURL, m))
		}
		date = takeSnapshot(t)
		record(t, "mid_run_snapshot", date)
		keep(t, "snapshots.txt", []byte(sh(t, time.Minute, "tmutil", "listlocalsnapshots", "/").out))
		end(t, id)
		deadline := time.Now().Add(3 * time.Minute)
		for {
			_, err := os.Stat(dir)
			if os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("run dir %s still exists 3 minutes after the run ended (%v)", dir, err)
			}
			time.Sleep(2 * time.Second)
		}
		// The suite's own logs and evidence hold the marker; they are not the run's leftovers.
		own, _ := filepath.Abs(filepath.Dir(cfg.evidence))
		for _, p := range strings.Split(strings.TrimSpace(sh(t, 2*time.Minute, "mdfind", m).out), "\n") {
			if p != "" && !strings.HasPrefix(p, own+string(filepath.Separator)) {
				t.Errorf("Spotlight finds the marker outside the suite's own output: %s", p)
			}
		}
		if ex := sh(t, time.Minute, "tmutil", "isexcluded", layout(t).Root).out; !strings.Contains(ex, "[Excluded]") {
			t.Errorf("tmutil does not report %s excluded: %s", layout(t).Root, ex)
		}
		t.Run("safari_cookie", func(t *testing.T) {
			if cfg.cookieURL == "" || !cfg.prompts {
				notRun(t, "the Safari cookie needs WARDYN_MACVM_COOKIE_URL and a person")
			}
		})
		t.Run("not_allowed_agent", func(t *testing.T) {
			agent := os.Getenv("WARDYN_MACVM_NO_SIGNIN_AGENT")
			need(t, agent, "WARDYN_MACVM_NO_SIGNIN_AGENT (an agent that does not allow sign-ins)")
			mustRefuse(t, askRefused(t, spec{intent: map[string]any{"agent": agent}}), "placement", regexp.MustCompile(`does not allow sign-ins in a desktop`))
		})
	})
	t.Run("snapshot_findings", func(t *testing.T) {
		if date == "" {
			notRun(t, "no mid-run snapshot was made")
		}
		defer deleteSnapshot(t, date)
		if !cfg.prompts {
			notRun(t, "a person mounts the snapshot read-only and searches it (WARDYN_MACVM_PROMPTS=1)")
		}
		record(t, "marker_in_mid_run_snapshot", ask(t, "Mount snapshot "+date+" read-only and search it for "+m+": found or not found?"))
		record(t, "marker_in_other_snapshots", ask(t, "Search any other listed snapshot for it: found, not found, or none listed?"))
	})
}

// TestP11_CAAndTools: the works/fails matrix under a door that uses MITM. The matrix is the result
// (a limit the release note must match); each tool's own exit status decides works or fails.
func TestP11_CAAndTools(t *testing.T) {
	live(t)
	doc := os.Getenv("WARDYN_MACVM_MITM_RUN_DOC")
	need(t, doc, "WARDYN_MACVM_MITM_RUN_DOC (a kind: Run document whose policy's door uses MITM)")
	need(t, cfg.allowedURL, "WARDYN_MACVM_ALLOWED_URL")
	id := start(t, spec{doc: doc})
	u := cfg.allowedURL
	git := env("WARDYN_MACVM_GIT_URL", u)
	matrix := map[string]string{}
	for _, c := range []struct{ name, script, ok string }{
		{"node", `node -e 'require("https").get(process.argv[1], r => { console.log(r.statusCode); process.exit(r.statusCode >= 200 && r.statusCode < 300 ? 0 : 1) }).on("error", e => { console.error(e.message); process.exit(1) })' '` + u + `'`, `^2\d\d$`},
		{"git", "git ls-remote '" + git + "' >/dev/null && echo ok", `^ok$`},
		{"curl", "curl -sS -o /dev/null -w '%{http_code}' '" + u + "'", `^2\d\d$`},
		{"python3", `python3 -c 'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1]).status)' '` + u + `'`, `^2\d\d$`},
	} {
		r := guest(t, id, c.script, 2*time.Minute)
		works := r.code == 0 && regexp.MustCompile(c.ok).MatchString(strings.TrimSpace(r.out))
		matrix[c.name] = map[bool]string{true: "works", false: fmt.Sprintf("fails: exit %d %s", r.code, clean(r.out+" "+r.err))}[works]
	}
	t.Run("safari", func(t *testing.T) {
		if !cfg.prompts {
			notRun(t, "Safari is a person's check (WARDYN_MACVM_PROMPTS=1)")
		}
		matrix["safari"] = ask(t, fmt.Sprintf("In run %s's guest, open %s in Safari: works or fails (with the error)?", id, u))
	})
	b, _ := json.MarshalIndent(matrix, "", "  ")
	keep(t, "matrix.json", b)
	t.Logf("RECORD matrix=%s", b)
}
