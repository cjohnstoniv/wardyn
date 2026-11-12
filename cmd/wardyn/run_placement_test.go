// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunPlacementFlagsAreSentUnchanged(t *testing.T) {
	runnerID := uuid.New()

	t.Run("placement local with runner", func(t *testing.T) {
		for _, c := range []struct {
			name, path string
			extra      []string
		}{
			{"create", "/api/v1/runs", nil},
			{"dry-run", "/api/v1/runs/preflight", []string{"--dry-run"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				args := append([]string{"run", "--agent", "claude-code", "--placement", "local", "--runner", runnerID.String()}, c.extra...)
				srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "invalid_request"})
				if err := execCmd(t, append(args, "--url", srv.URL, "--token", "t")...); err == nil {
					t.Fatal("the server's refusal was swallowed")
				}
				if got := srv.last().path; got != c.path {
					t.Errorf("path = %s, want %s", got, c.path)
				}
				var body sdk.CreateRunRequest
				if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "local" || body.RunnerID != runnerID.String() {
					t.Fatalf("body = %s, want placement=local runner_id=%s", srv.last().body, runnerID)
				}
			})
		}
	})

	t.Run("invalid values pass through", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusBadRequest, map[string]string{"error": "x", "reason": "invalid_request"})
		if err := execCmd(t, "run", "--agent", "claude-code", "--placement", "nowhere", "--runner", "not-a-uuid", "--url", srv.URL, "--token", "t"); err == nil {
			t.Fatal("invalid placement/runner should return an error from the server")
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "nowhere" || body.RunnerID != "not-a-uuid" {
			t.Fatalf("body = %s, want placement=nowhere runner_id=not-a-uuid", srv.last().body)
		}
	})

	t.Run("runner without placement local passes through", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "invalid_request"})
		if err := execCmd(t, "run", "--agent", "claude-code", "--runner", runnerID.String(), "--url", srv.URL, "--token", "t"); err == nil {
			t.Fatal("the server's refusal was swallowed")
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.RunnerID != runnerID.String() || body.Placement != "" {
			t.Fatalf("body = %s, want runner_id=%s and no placement", srv.last().body, runnerID)
		}
	})

	t.Run("neither flag leaves the keys out", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		if err := execCmd(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"); err != nil {
			t.Fatalf("run without placement/runner returned error: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(srv.last().body, &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		for _, key := range []string{"placement", "runner_id"} {
			if _, ok := raw[key]; ok {
				t.Errorf("%s key present in body when unset: %v", key, raw)
			}
		}
	})
}

func TestRunMachineFlagsAreSentUnchanged(t *testing.T) {
	t.Run("kind and base", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		if err := execCmd(t, "run", "--agent", "claude-code", "--machine-kind", "macos_vm", "--macos-base", "Xcode", "--url", srv.URL, "--token", "t"); err != nil {
			t.Fatal(err)
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.MachineKind != "macos_vm" || body.MacOSBase != "Xcode" {
			t.Fatalf("body = %s, want machine_kind=macos_vm macos_base=Xcode", srv.last().body)
		}
	})

	t.Run("neither flag leaves the keys out", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		if err := execCmd(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(srv.last().body, &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		for _, key := range []string{"machine_kind", "macos_base"} {
			if _, ok := raw[key]; ok {
				t.Errorf("%s key present in body when unset: %v", key, raw)
			}
		}
	})
}

// The keep flags map one for one onto the request's machine field.
func TestRunKeptMachineFlagsBuildTheMachineField(t *testing.T) {
	base := []string{"run", "--experience", "interactive", "--machine-kind", "macos_vm"}
	for _, tc := range []struct {
		name  string
		flags []string
		want  *sdk.RunMachine
	}{
		{"keep", []string{"--keep-machine"}, &sdk.RunMachine{Keep: true}},
		{"keep with a name", []string{"--keep-machine", "--machine-name", "dev"}, &sdk.RunMachine{Keep: true, Name: "dev"}},
		{"resume by name", []string{"--resume-machine", "dev"}, &sdk.RunMachine{Resume: "dev"}},
		{"resume by id", []string{"--resume-machine", "7f3c2a91"}, &sdk.RunMachine{Resume: "7f3c2a91"}},
		{"resume fresh", []string{"--resume-machine", "dev", "--fresh"}, &sdk.RunMachine{Resume: "dev", Fresh: true}},
		{"no keep flag", nil, nil},
	} {
		for _, dry := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
				args := append(append(append([]string{}, base...), tc.flags...), "--agent", "claude-code", "--url", srv.URL, "--token", "t")
				wantPath := "/api/v1/runs"
				if dry {
					args, wantPath = append(args, "--dry-run"), "/api/v1/runs/preflight"
				}
				_ = execCmd(t, args...)
				// A created run is followed by a read of the machine list, so the request is found by its path.
				create := srv.sentTo(wantPath)
				if create == nil {
					t.Fatalf("no request to %q", wantPath)
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(create, &raw); err != nil {
					t.Fatal(err)
				}
				if tc.want == nil {
					if _, ok := raw["machine"]; ok {
						t.Fatalf("machine key present with no keep flag: %s", create)
					}
					return
				}
				var body sdk.CreateRunRequest
				if err := json.Unmarshal(create, &body); err != nil || body.Machine == nil || *body.Machine != *tc.want {
					t.Fatalf("machine = %v (%v), want %+v; body %s", body.Machine, err, *tc.want, create)
				}
			})
		}
	}
}

// Flags that contradict each other are refused with a sentence before any request goes out.
func TestRunKeptMachineFlagsRefuseContradictions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"keep and resume", []string{"--keep-machine", "--resume-machine", "dev"},
			"--keep-machine and --resume-machine can't go together: --keep-machine keeps a new machine, and --resume-machine starts from one you kept (it is kept again when the run stops)"},
		{"name with resume", []string{"--resume-machine", "dev", "--machine-name", "web"},
			"--machine-name names a new machine and can't go with --resume-machine, which already names the machine to resume"},
		{"name without keep", []string{"--machine-name", "web"},
			"--machine-name names the machine --keep-machine keeps: add --keep-machine, or drop --machine-name"},
		{"fresh without resume", []string{"--fresh"},
			"--fresh starts a machine you resume over again: add --resume-machine <id|name>, or drop --fresh"},
		{"fresh with keep", []string{"--keep-machine", "--fresh"},
			"--fresh starts a machine you resume over again: add --resume-machine <id|name>, or drop --fresh"},
		{"keep with a file", []string{"--file", "run.yaml", "--keep-machine"},
			"a run document can't keep or resume a machine: use the flags for that run instead of --file"},
		{"resume with a file", []string{"--file", "run.yaml", "--resume-machine", "dev"},
			"a run document can't keep or resume a machine: use the flags for that run instead of --file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
			err := execCmd(t, append(append([]string{"run", "--agent", "claude-code"}, tc.flags...), "--url", srv.URL, "--token", "t")...)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if p := srv.last().path; p != "" {
				t.Errorf("a refused run still sent %s", p)
			}
		})
	}
}

// The server judges everything else about a kept machine, and its sentence reaches the person as sent.
func TestRunKeptMachineServerRefusalIsShownUnchanged(t *testing.T) {
	const sentence = "Only an interactive macOS VM run can keep or resume a machine."
	srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": sentence, "reason": "kept_machine_interactive_only"})
	err := execCmd(t, "run", "--experience", "background", "--machine-kind", "macos_vm", "--keep-machine", "--task", "make test", "--agent", "claude-code", "--url", srv.URL, "--token", "t")
	if err == nil || err.Error() != "API error 422: "+sentence {
		t.Fatalf("err = %v, want the server's sentence unchanged", err)
	}
}

func TestRunKeptMachineHelpNamesTheModeFlags(t *testing.T) {
	root := rootCmd()
	run, _, err := root.Find([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep-machine", "resume-machine"} {
		f := run.Flags().Lookup(name)
		if f == nil || !strings.Contains(f.Usage, "--experience interactive --machine-kind macos_vm") {
			t.Errorf("--%s help does not name the flags it needs: %v", name, f)
		}
	}
	for _, name := range []string{"machine-name", "fresh"} {
		if run.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not a flag of wardyn run", name)
		}
	}
}

// A run that keeps or resumes a machine says so right under its created line; any other run says nothing
// more, and --json stays one JSON object.
func TestRunPrintsTheKeptMachineLine(t *testing.T) {
	dev := keptMachine("dev", types.KeptMachineKept)
	rig := func(t *testing.T) *routeRig {
		return newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
			"POST /api/v1/runs":     replyJSON(http.StatusCreated, sdk.CreateRunResult{AgentRun: types.AgentRun{ID: uuid.New(), State: types.RunQueued, ConfinementClass: "CC3"}}),
			"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines(dev)),
		})
	}
	used := machinesUsed.Local().Format("2006-01-02")
	base := []string{"run", "--experience", "interactive", "--machine-kind", "macos_vm", "--agent", "claude-code"}
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"keep", []string{"--keep-machine", "--machine-name", "web"}, "machine web: new, kept when this run stops"},
		{"keep unnamed", []string{"--keep-machine"}, "machine: new, kept when this run stops; it is named when the run starts"},
		{"resume", []string{"--resume-machine", "dev"}, "machine dev: will resume (last used " + used + "), kept when this run stops"},
		{"fresh", []string{"--resume-machine", "dev", "--fresh"}, "machine dev: will start fresh from Maya's MacBook's current base, kept when this run stops"},
		{"ordinary", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runMachines(t, rig(t), "", append(append([]string{}, base...), tc.flags...)...)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if !strings.HasPrefix(lines[0], "created run ") {
				t.Fatalf("first line = %q", lines[0])
			}
			if tc.want == "" {
				for _, l := range lines {
					if strings.HasPrefix(l, "machine") {
						t.Errorf("an ordinary run printed %q", l)
					}
				}
				return
			}
			if len(lines) < 2 || lines[1] != tc.want {
				t.Errorf("second line of\n%s\nwant %q", out, tc.want)
			}
		})
	}

	t.Run("json", func(t *testing.T) {
		r := rig(t)
		out, err := runMachines(t, r, "", append(append([]string{}, base...), "--resume-machine", "dev", "--json")...)
		if err != nil {
			t.Fatal(err)
		}
		var run types.AgentRun
		if err := json.Unmarshal([]byte(out), &run); err != nil || strings.Contains(out, "machine dev") {
			t.Errorf("--json stdout is not one run object: %q (%v)", out, err)
		}
		if n := len(r.sent("GET " + myMachinesPath)); n != 0 {
			t.Errorf("--json read the machine list %d times, want 0", n)
		}
	})
}

// sentTo is the body of the last request the server got at path, nil when none came.
func (s *cmdServer) sentTo(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reqs) - 1; i >= 0; i-- {
		if s.reqs[i].path == path {
			return s.reqs[i].body
		}
	}
	return nil
}
