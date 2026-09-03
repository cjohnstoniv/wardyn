// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestStateStoreTableCoversUserDrives pins the top of the operator manual — the
// state-store table and the backup runbook under it — to the storage a drive
// actually creates.
//
// That table opens "hold data that exists nowhere else. Lose any of them and the
// loss is permanent", and it is the first thing an operator reads about what to
// back up. A user drive is exactly that kind of data — a person's own files,
// which Postgres never holds (it holds the drive rows and the allocations) — so
// a table that lists only Postgres, recordings and the age key sends an operator
// away believing pg_dump covered everything. Worse on Docker: `wardyn-drive-*`
// volumes are created through the Docker API rather than declared in the compose
// file, so they are invisible both to a compose-volume backup AND to
// `compose down -v`, and nothing said so.
//
// The expected object shapes are DERIVED from types.DriveObjectName by feeding
// it the doc's own placeholders, so a rename of the object-naming scheme fails
// here rather than leaving an operator grepping for a volume that no longer
// exists under that name.
func TestStateStoreTableCoversUserDrives(t *testing.T) {
	doc := readDoc(t, "docs/OPERATIONS.md")

	// DriveObjectName is total over the backends, and it composes its answer
	// from the inputs — so the doc's placeholder shapes ARE its output on
	// placeholder inputs.
	// A NAMED drive, because a nameless one is not a drive: DriveObjectName is
	// prefix + driveSlug(Name) + "-" + home, so feeding the zero UserDrive
	// derived "wardyn-drive--<home>" — a double hyphen no valid row can produce
	// (ValidateUserDrive requires a name) and which therefore appears in no
	// document. Derive with a real slug and swap it for the doc's placeholder:
	// the shape stays derived from the function, so a rename still fails here,
	// and the expectation is now a name an operator could actually grep for.
	//
	// The docker-volume and PVC backends share DriveObjectName's default arm,
	// so they share one shape; only host_path differs.
	const probeSlug = "probe-drive"
	derive := func(d types.UserDrive) string {
		return strings.Replace(types.DriveObjectName(d, "<home>"), probeSlug, "<drive-slug>", 1)
	}
	volume := derive(types.UserDrive{Backend: types.DriveBackendDockerVolume, Name: probeSlug})
	hostPath := derive(types.UserDrive{Backend: types.DriveBackendHostPath, HostRoot: "<host_root>", Name: probeSlug})
	pvc := derive(types.UserDrive{Backend: types.DriveBackendK8sPVC, Name: probeSlug})

	// Scoped to the ROW, not the document. Both minted backends now share one
	// name, so a document-wide Contains is answered by the Kubernetes sections
	// three thousand lines away and the row's own shape could be wrong while
	// this passed — which is exactly what happened when the naming rule changed:
	// this guard stayed green on a state-store row naming a volume that no
	// longer exists.
	row := driveStateStoreRow(t)
	for _, want := range []string{volume, hostPath, pvc} {
		if !strings.Contains(row, want) {
			t.Errorf("docs/OPERATIONS.md's state-store User drives row never names the object shape %q; row is:\n%s", want, row)
		}
	}
	// Every backend must be reachable from one of the shapes above; a NEW
	// backend with a new shape has to be documented, not silently added.
	if len(types.DriveBackends) != 4 {
		t.Errorf("types.DriveBackends now has %d entries — the state-store row documents 3 object shapes for 4 backends "+
			"(the two PVC backends share one); re-check the row before widening this guard", len(types.DriveBackends))
	}
	for _, want := range []struct{ claim, why string }{
		{"`pg_dump` never carried this",
			"Postgres holds the drive rows and allocations, never the bytes"},
		{"leaves every one of them in place",
			"`wardyn-drive-*` volumes are not in the compose project, so `compose down -v` does not reach them"},
		{"label=wardyn.managed=true",
			"the only way to list the managed volumes a backup has to walk"},
		{"step 1's dump does NOT contain\n#    them",
			"the backup runbook must say the drives are not in the Postgres dump"},
	} {
		if !strings.Contains(doc, strings.Join(strings.Fields(want.claim), " ")) {
			t.Errorf("docs/OPERATIONS.md's state-store section omits %q — %s", want.claim, want.why)
		}
	}
}

// driveStateStoreRow returns the "User drives" row of the State stores table,
// read from the raw file so the row boundary survives (readDoc collapses
// newlines for prose matching, which would merge this row into its neighbours).
func driveStateStoreRow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "OPERATIONS.md"))
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| User drives |") {
			return line
		}
	}
	t.Fatal("docs/OPERATIONS.md's State stores table has no `User drives` row")
	return ""
}
