// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const myMachinesPath = "/api/v1/me/machines"

var (
	machinesUsed    = time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	machinesExpires = time.Now().UTC().Truncate(time.Hour).Add(30 * 24 * time.Hour)
)

func keptMachine(name string, state types.KeptMachineState) sdk.KeptMachine {
	return sdk.KeptMachine{
		ID: uuid.New(), Name: name, RunnerID: uuid.New(), RunnerName: "Maya's MacBook", Base: "Standard",
		State: state, Scrubbed: true, DiskBytes: 12 << 30, LastUsedAt: machinesUsed, ExpiresAt: machinesExpires,
	}
}

func myMachines(ms ...sdk.KeptMachine) sdk.MyMachines {
	return sdk.MyMachines{Machines: append([]sdk.KeptMachine{}, ms...), Allowed: true, Count: len(ms), Limit: 2}
}

// runMachines runs args with in as stdin and returns what the command printed to stdout.
func runMachines(t *testing.T, rig *routeRig, in string, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	var out strings.Builder
	root.SetArgs(append(args, "--url", rig.URL, "--token", "tok"))
	root.SetIn(strings.NewReader(in))
	root.SetOut(&out)
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	return out.String(), err
}

func withTerminal(t *testing.T, is bool) {
	t.Helper()
	orig := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = orig })
	stdinIsTerminal = func() bool { return is }
}

func TestMachinesListShowsEachMachine(t *testing.T) {
	dev := keptMachine("dev", types.KeptMachineKept)
	busy := keptMachine("design-review", types.KeptMachineInUse)
	busy.Scrubbed, busy.DiskBytes, busy.Base = false, 31<<30, "Xcode 26"
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines(dev, busy)),
	})
	out, err := runMachines(t, rig, "", "machines")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header and two rows, got:\n%s", out)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME STATE MAC BASE LAST USED EXPIRES WRITTEN SCRUBBED" {
		t.Errorf("header = %q", lines[0])
	}
	used, expires := machinesUsed.Local().Format("2006-01-02"), machinesExpires.Local().Format("2006-01-02")
	for line, want := range map[int]string{
		1: "dev kept Maya's MacBook Standard " + used + " " + expires + " 12 GB yes",
		2: "design-review in_use Maya's MacBook Xcode 26 " + used + " " + expires + " 31 GB no",
	} {
		if got := strings.Join(strings.Fields(lines[line]), " "); got != want {
			t.Errorf("row %d = %q, want %q", line, got, want)
		}
	}
}

func TestMachinesListNoneAndNoMacName(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines()),
	})
	out, err := runMachines(t, rig, "", "machines")
	if err != nil || out != "no kept machines\n" {
		t.Fatalf("out = %q, err = %v, want \"no kept machines\"", out, err)
	}

	m := keptMachine("dev", types.KeptMachineKept)
	m.RunnerName, m.DiskBytes, m.Base = "", 0, ""
	var sb strings.Builder
	if err := writeMachines(&sb, []sdk.KeptMachine{m}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), m.RunnerID.String()) {
		t.Errorf("a machine with no Mac name should show its runner id:\n%s", sb.String())
	}
}

func TestMachinesListSaysWhatADeletingMachineWaitsFor(t *testing.T) {
	expired := keptMachine("old", types.KeptMachineDeleting)
	expired.DeleteReason = sdk.KeptMachineDeletedByExpired
	deleting := keptMachine("dev", types.KeptMachineDeleting)
	deleting.DeleteReason = sdk.KeptMachineDeletedByPerson
	var sb strings.Builder
	if err := writeMachines(&sb, []sdk.KeptMachine{expired, deleting}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"old", "expired",
		"machine old: This machine expired and will be deleted.\n",
		"Your machine dev is being deleted.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"was deleted", "has been deleted", "deleted machine"} {
		if strings.Contains(out, bad) {
			t.Errorf("a pending delete must not read as done (%q):\n%s", bad, out)
		}
	}
}

func TestMachinesListJSONAndRefusal(t *testing.T) {
	m := keptMachine("dev", types.KeptMachineKept)
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines(m)),
	})
	out, err := runMachines(t, rig, "", "machines", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got sdk.MyMachines
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Machines) != 1 || got.Machines[0].Name != "dev" || got.Limit != 2 {
		t.Fatalf("--json = %s (%v)", out, err)
	}

	const sentence = "Wardyn keeps a machine only on a Mac whose runner you registered, for runs you start yourself."
	rig = newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusForbidden, map[string]string{"error": sentence, "reason": "kept_machine_not_your_mac"}),
	})
	if _, err = runMachines(t, rig, "", "machines"); err == nil || err.Error() != "API error 403: "+sentence {
		t.Fatalf("err = %v, want the server's sentence unchanged", err)
	}
}

// deleteRig serves the list (listed first, then after, once the delete was asked) and the delete.
func deleteRig(t *testing.T, m sdk.KeptMachine, del func(http.ResponseWriter, []byte), after sdk.MyMachines) *routeRig {
	t.Helper()
	deleted := false
	return newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: func(w http.ResponseWriter, b []byte) {
			if deleted {
				replyJSON(http.StatusOK, after)(w, b)
				return
			}
			replyJSON(http.StatusOK, myMachines(m))(w, b)
		},
		"DELETE " + myMachinesPath + "/" + m.ID.String(): func(w http.ResponseWriter, b []byte) {
			deleted = true
			del(w, b)
		},
	})
}

func noContent(w http.ResponseWriter, _ []byte) { w.WriteHeader(http.StatusNoContent) }

func TestMachinesDeleteByNameAndByID(t *testing.T) {
	for _, byID := range []bool{false, true} {
		m := keptMachine("dev", types.KeptMachineKept)
		rig := deleteRig(t, m, noContent, myMachines())
		ref := "dev"
		if byID {
			ref = m.ID.String()
		}
		out, err := runMachines(t, rig, "", "machines", "delete", ref, "--yes")
		if err != nil {
			t.Fatalf("delete %s: %v", ref, err)
		}
		if want := "deleted machine dev from Maya's MacBook\n"; out != want {
			t.Errorf("delete %s printed %q, want %q", ref, out, want)
		}
		if n := len(rig.sent("DELETE " + myMachinesPath + "/" + m.ID.String())); n != 1 {
			t.Errorf("delete %s sent %d deletes, want 1", ref, n)
		}
	}
}

func TestMachinesDeleteDeferredSaysItIsNotDone(t *testing.T) {
	m := keptMachine("dev", types.KeptMachineKept)
	still := m
	still.State = types.KeptMachineDeleting
	rig := deleteRig(t, m, replyJSON(http.StatusAccepted, still), myMachines(still))
	out, err := runMachines(t, rig, "", "machines", "delete", "dev", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if want := "machine dev: Maya's MacBook is offline; it is deleted when it's back\n"; out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	if strings.Contains(out, "deleted machine") {
		t.Errorf("a delete waiting for its Mac must not say it was deleted: %q", out)
	}
}

func TestMachinesDeleteWhenTheAfterListCannotBeRead(t *testing.T) {
	m := keptMachine("dev", types.KeptMachineKept)
	reads := 0
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: func(w http.ResponseWriter, b []byte) {
			if reads++; reads > 1 {
				http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
				return
			}
			replyJSON(http.StatusOK, myMachines(m))(w, b)
		},
		"DELETE " + myMachinesPath + "/" + m.ID.String(): noContent,
	})
	out, err := runMachines(t, rig, "", "machines", "delete", "dev", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "deleted machine") || !strings.Contains(out, "run 'wardyn machines' to see whether it is done") {
		t.Errorf("an unreadable result must not claim the delete: %q", out)
	}
}

func TestMachinesDeleteShowsTheServersRefusalUnchanged(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		reason   string
		sentence string
	}{
		{"in use", http.StatusConflict, "kept_machine_in_use", "This machine is in use by a run. End that run first, then delete it."},
		{"already deleting", http.StatusConflict, "kept_machine_deleting", "Your machine dev is being deleted."},
		{"unconfirmed", http.StatusServiceUnavailable, "kept_machine_delete_unconfirmed", "Wardyn couldn't confirm the delete. It will keep trying; the machine can't be resumed meanwhile."},
		{"mac refused", http.StatusServiceUnavailable, "kept_machine_delete_failed", "The machine couldn't be deleted from Maya's MacBook. Nothing was removed; try again."},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := keptMachine("dev", types.KeptMachineKept)
			rig := deleteRig(t, m, replyJSON(c.status, map[string]string{"error": c.sentence, "reason": c.reason}), myMachines(m))
			out, err := runMachines(t, rig, "", "machines", "delete", "dev", "--yes")
			if want := fmt.Sprintf("API error %d: %s", c.status, c.sentence); err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if out != "" {
				t.Errorf("a refused delete printed %q on stdout", out)
			}
		})
	}
}

// A name is matched in any case, as the server matches it, and the delete is still confirmed first.
func TestMachinesDeleteFindsANameInAnyCase(t *testing.T) {
	withTerminal(t, false)
	m := keptMachine("dev", types.KeptMachineKept)
	rig := deleteRig(t, m, noContent, myMachines())
	_, err := runMachines(t, rig, "", "machines", "delete", "DEV")
	if err == nil || !strings.Contains(err.Error(), "nothing was deleted: without a terminal, pass --yes to delete machine dev.") {
		t.Fatalf("err = %v", err)
	}
	path := "DELETE " + myMachinesPath + "/" + m.ID.String()
	if n := len(rig.sent(path)); n != 0 {
		t.Fatalf("sent %d deletes before the confirm, want none", n)
	}
	if _, err := runMachines(t, rig, "", "machines", "delete", "Dev", "--yes"); err != nil {
		t.Fatalf("delete Dev: %v", err)
	}
	if n := len(rig.sent(path)); n != 1 {
		t.Errorf("sent %d deletes, want 1", n)
	}
}

func TestMachinesDeleteAsksFirst(t *testing.T) {
	path := func(m sdk.KeptMachine) string { return "DELETE " + myMachinesPath + "/" + m.ID.String() }

	t.Run("no terminal and no --yes refuses", func(t *testing.T) {
		withTerminal(t, false)
		m := keptMachine("dev", types.KeptMachineKept)
		rig := deleteRig(t, m, noContent, myMachines())
		_, err := runMachines(t, rig, "", "machines", "delete", "dev")
		if err == nil || !strings.Contains(err.Error(), "nothing was deleted: without a terminal, pass --yes to delete machine dev.") {
			t.Fatalf("err = %v", err)
		}
		if n := len(rig.sent(path(m))); n != 0 {
			t.Errorf("sent %d deletes, want none", n)
		}
	})

	t.Run("yes at the prompt deletes", func(t *testing.T) {
		withTerminal(t, true)
		m := keptMachine("dev", types.KeptMachineKept)
		rig := deleteRig(t, m, noContent, myMachines())
		out, err := runMachines(t, rig, "y\n", "machines", "delete", "dev")
		if err != nil {
			t.Fatal(err)
		}
		const ask = "Delete machine dev? This deletes it from Maya's MacBook, with the files, tools and sign-ins on it. It can't be undone. [y/N] "
		if !strings.HasPrefix(out, ask) || !strings.HasSuffix(out, "deleted machine dev from Maya's MacBook\n") {
			t.Errorf("printed %q", out)
		}
	})

	t.Run("anything else at the prompt keeps it", func(t *testing.T) {
		withTerminal(t, true)
		for _, in := range []string{"\n", "n\n", ""} {
			m := keptMachine("dev", types.KeptMachineKept)
			rig := deleteRig(t, m, noContent, myMachines())
			_, err := runMachines(t, rig, in, "machines", "delete", "dev")
			if err == nil || err.Error() != "nothing was deleted" {
				t.Errorf("answer %q: err = %v", in, err)
			}
			if n := len(rig.sent(path(m))); n != 0 {
				t.Errorf("answer %q: sent %d deletes, want none", in, n)
			}
		}
	})
}

// every spelling uuid.Parse reads, for one id
func uuidSpellings(id uuid.UUID) map[string]string {
	canon := id.String()
	return map[string]string{
		"canonical": canon,
		"upper":     strings.ToUpper(canon),
		"braced":    "{" + canon + "}",
		"urn":       "urn:uuid:" + canon,
		"32 hex":    strings.ReplaceAll(canon, "-", ""),
	}
}

// Whatever spelling names your machine, the delete is asked about first, and sent once, and only then.
func TestMachinesDeleteOfAnyIDSpellingIsGated(t *testing.T) {
	for _, form := range []string{"canonical", "upper", "braced", "urn", "32 hex"} {
		t.Run(form, func(t *testing.T) {
			m := keptMachine("dev", types.KeptMachineKept)
			ref := uuidSpellings(m.ID)[form]
			del := "DELETE " + myMachinesPath + "/" + m.ID.String()

			withTerminal(t, false)
			rig := deleteRig(t, m, noContent, myMachines())
			if _, err := runMachines(t, rig, "", "machines", "delete", ref); err == nil || !strings.Contains(err.Error(), "pass --yes") {
				t.Errorf("no terminal, no --yes: err = %v", err)
			}
			if n := len(rig.sent(del)); n != 0 {
				t.Errorf("no terminal, no --yes: %d DELETEs, want 0", n)
			}

			withTerminal(t, true)
			rig = deleteRig(t, m, noContent, myMachines())
			if _, err := runMachines(t, rig, "n\n", "machines", "delete", ref); err == nil || err.Error() != "nothing was deleted" {
				t.Errorf("answer n: err = %v", err)
			}
			if n := len(rig.sent(del)); n != 0 {
				t.Errorf("answer n: %d DELETEs, want 0", n)
			}

			rig = deleteRig(t, m, noContent, myMachines())
			out, err := runMachines(t, rig, "", "machines", "delete", ref, "--yes")
			if err != nil || out != "deleted machine dev from Maya's MacBook\n" {
				t.Errorf("--yes: out %q, err %v", out, err)
			}
			if n := len(rig.sent(del)); n != 1 {
				t.Errorf("--yes: %d DELETEs, want exactly 1", n)
			}
		})
	}
}

// A reference that names none of your machines is refused here: no DELETE goes out for the server to answer.
func TestMachinesDeleteUnknownMachineSendsNothing(t *testing.T) {
	other := uuid.New()
	for _, ref := range []string{"nope", other.String(), strings.ToUpper(other.String()), "urn:uuid:" + other.String()} {
		for _, yes := range []bool{false, true} {
			withTerminal(t, true)
			rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
				"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines(keptMachine("dev", types.KeptMachineKept))),
			})
			args := []string{"machines", "delete", ref}
			if yes {
				args = append(args, "--yes")
			}
			_, err := runMachines(t, rig, "y\n", args...)
			if err == nil || err.Error() != "You have no kept machine "+ref+"." {
				t.Errorf("%s (yes=%v): err = %v", ref, yes, err)
			}
			for key := range rig.bodies {
				if strings.HasPrefix(key, "DELETE ") {
					t.Errorf("%s (yes=%v): sent %s", ref, yes, key)
				}
			}
		}
	}
}

func TestMachinesDeleteTakesExactlyOneMachine(t *testing.T) {
	rig := newRouteRig(t, nil)
	for _, args := range [][]string{{"machines", "delete"}, {"machines", "delete", "a", "b"}} {
		if _, err := runMachines(t, rig, "", args...); err == nil {
			t.Errorf("wardyn %s exited 0", strings.Join(args, " "))
		}
	}
}

func TestMachineCreatedLine(t *testing.T) {
	dev := keptMachine("dev", types.KeptMachineKept)
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusOK, myMachines(dev)),
	})
	c := &sdk.Client{BaseURL: rig.URL, Token: "tok", HTTPClient: rig.Client()}
	used := machinesUsed.Local().Format("2006-01-02")
	for _, tc := range []struct {
		name string
		m    *sdk.RunMachine
		want string
	}{
		{"no machine", nil, ""},
		{"keep named", &sdk.RunMachine{Keep: true, Name: "web"}, "machine web: new, kept when this run stops"},
		{"keep unnamed", &sdk.RunMachine{Keep: true}, "machine: new, kept when this run stops; it is named when the run starts"},
		{"resume by name", &sdk.RunMachine{Resume: "dev"}, "machine dev: will resume (last used " + used + "), kept when this run stops"},
		{"resume by id", &sdk.RunMachine{Resume: dev.ID.String()}, "machine dev: will resume (last used " + used + "), kept when this run stops"},
		{"fresh", &sdk.RunMachine{Resume: "dev", Fresh: true}, "machine dev: will start fresh from Maya's MacBook's current base, kept when this run stops"},
		{"resume, not in the list", &sdk.RunMachine{Resume: "7f3c2a91"}, "machine 7f3c2a91: will resume, kept when this run stops"},
		{"fresh, not in the list", &sdk.RunMachine{Resume: "7f3c2a91", Fresh: true}, "machine 7f3c2a91: will start fresh from its Mac's current base, kept when this run stops"},
	} {
		if got := machineCreatedLine(context.Background(), c, tc.m); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	down := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"GET " + myMachinesPath: replyJSON(http.StatusServiceUnavailable, map[string]string{"error": "down"}),
	})
	c = &sdk.Client{BaseURL: down.URL, Token: "tok", HTTPClient: down.Client()}
	if got, want := machineCreatedLine(context.Background(), c, &sdk.RunMachine{Resume: "dev"}), "machine dev: will resume, kept when this run stops"; got != want {
		t.Errorf("list down: %q, want %q", got, want)
	}
}

func TestMachineGBRoundsToTheWholeGB(t *testing.T) {
	for bytes, want := range map[int64]string{
		0: "-", -1: "-", 1: "<1 GB", 1<<29 - 1: "<1 GB", 1 << 29: "1 GB", 12 << 30: "12 GB", 51<<30 + 1<<29: "52 GB",
	} {
		if got := machineGB(bytes); got != want {
			t.Errorf("machineGB(%d) = %q, want %q", bytes, got, want)
		}
	}
}
