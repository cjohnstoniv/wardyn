// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// recoveryMode dispatches the recovery-set verbs (docs/design/0.9/recovery-set.md §1.1), beside
// maintenanceMode. Each verb is a maintenance mode of its own: it refuses to run with another verb or with
// any other maintenance mode, and has no env pair.
func recoveryMode(f *bootFlags) (bool, error) {
	verbs := 0
	for _, on := range []bool{*f.recoveryInventory, *f.recoveryManifest, *f.recoveryVerify, *f.recoveryRehearse} {
		if on {
			verbs++
		}
	}
	if verbs == 0 {
		return false, nil
	}
	if verbs > 1 || strings.TrimSpace(*f.rotateAgeKey) != "" || *f.rewrap || *f.rewrapPrincipalKeys ||
		*f.migrateSecrets || *f.reconcile || *f.auditSplitLegacy || *f.migrateOnly {
		return true, fmt.Errorf("refusing to run: each recovery verb (-recovery-inventory, -recovery-manifest, -recovery-verify, -recovery-rehearse) runs alone, with no other maintenance mode")
	}
	if *f.recoveryInventory {
		return true, writeRecoveryInventory(os.Stdout, f, os.Getenv("WARDYN_MANAGED_DIR"))
	}
	if *f.recoveryRehearse {
		return true, withBootTransport(f, recoveryRehearseMode)
	}
	return true, recoveryManifestMode(f, *f.recoveryVerify)
}

// The deployments the inventory describes. A deployment is inferred from the resolved runner and the
// managed directory: the Helm chart runs the Kubernetes runner, the managed desktop exports
// WARDYN_MANAGED_DIR, and anything else is Compose.
const (
	recoveryDeployCompose = "compose"
	recoveryDeployDesktop = "desktop"
	recoveryDeployHelm    = "helm"
)

// recoveryItem is one row of the set (recovery-set.md §2.1). Location names where the item lives for this
// deployment; Source names the setting, never its value; SetPath is where the item sits in the set.
type recoveryItem struct {
	Item     string `json:"item"`
	Location string `json:"location"`
	Source   string `json:"source"`
	SetPath  string `json:"set_path"`
	Note     string `json:"note"`
}

type recoveryInventoryReport struct {
	Deployment string         `json:"deployment"`
	Items      []recoveryItem `json:"items"`
}

// recoveryWhere is where each deployment keeps the state the set copies (docs/OPERATIONS.md, "Recovery set
// by deployment"). Keys are item names.
var recoveryWhere = map[string]map[string]string{
	recoveryDeployCompose: {
		"database":    "the postgres_data volume of the ${WARDYN_NS:-wardyn}-postgres container",
		"audit_spool": "the <project>_audit volume, mounted at /data/audit",
		"recordings":  "the ${WARDYN_NS:-wardyn}-recordings volume, under the fs store only",
		"drives":      "one Docker volume per person, labelled wardyn.managed=true",
		"keys":        "WARDYN_AGE_KEY in deploy/compose/.env; a key service is backed up separately",
	},
	recoveryDeployDesktop: {
		"database":    "the postgres_data volume of the wardyn-desktop Compose project; stop the converge job first",
		"audit_spool": "the audit volume of the wardyn-desktop project, mounted at /data/audit",
		"recordings":  "the recordings volume of the wardyn-desktop project, under the fs store only",
		"drives":      "one Docker volume per person, labelled wardyn.managed=true",
		"keys":        "age.key stays on the device; keep WARDYN_PLATFORM_KEY_FILE if it is set",
	},
	recoveryDeployHelm: {
		"database":    "the Postgres you operate (postgres.dsn); the chart renders none",
		"audit_spool": "the persistence volume at persistence.mountPath; a /tmp emptyDir when persistence is off",
		"recordings":  "the PVC, with persistence.enabled=true (the fs store)",
		"drives":      "one PVC per drive (a snapshot per claim)",
		"keys":        "the age-key Secret, or the key service for Vault Transit or Azure Key Vault",
	},
}

// recoveryDeployment names the deployment from the resolved runner and the managed directory.
func recoveryDeployment(runner, managedDir string) string {
	switch {
	case runner == "k8s":
		return recoveryDeployHelm
	case managedDir != "":
		return recoveryDeployDesktop
	}
	return recoveryDeployCompose
}

// writeRecoveryInventory prints the set's items as JSON. It reads resolved flags and the managed directory
// only: no connection is opened and no key is decrypted. The database appears as host, port and database
// name; the DSN's user, password and query parameters never leave this function, and neither does any key
// value. Only setting names appear in Source.
func writeRecoveryInventory(w io.Writer, f *bootFlags, managedDir string) error {
	dep := recoveryDeployment(*f.runnerSel, managedDir)
	rep := recoveryInventoryReport{Deployment: dep, Items: recoveryItems(f, dep)}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func recoveryItems(f *bootFlags, dep string) []recoveryItem {
	where := recoveryWhere[dep]
	spool := strings.TrimSpace(*f.auditSpool)
	spoolItem := recoveryItem{
		Item: "audit_spool", Location: "none: WARDYN_AUDIT_SPOOL is empty", Source: "WARDYN_AUDIT_SPOOL",
		Note: "no spool to collect",
	}
	if spool != "" {
		base := filepath.Base(spool)
		spoolItem = recoveryItem{
			Item: "audit_spool", Location: where["audit_spool"] + "; file " + spool, Source: "WARDYN_AUDIT_SPOOL",
			SetPath: "audit/" + base,
			Note:    "with the " + base + ".consumed and " + base + ".quarantine sidecars at audit/; each may be absent",
		}
	}
	return []recoveryItem{
		{
			Item: "database", Location: where["database"] + "; " + dsnDisplay(*f.dsn), Source: "WARDYN_PG_DSN",
			Note: "the pg_dump or provider snapshot, kept outside the set; identified by its audit head",
		},
		spoolItem,
		recordingsItem(f, where),
		{
			Item: "drives", Location: where["drives"], Source: "user_drives and user_drive_grants in the database",
			SetPath: "drives/<drive id>/<home>/",
			Note:    "one object per drive and home, named by types.DriveObjectName; a host_path drive is a subtree of its share",
		},
		{
			Item: "keys", Location: where["keys"], Source: keySources(f),
			Note: "never in the set; the manifest holds fingerprints and row counts only",
		},
		{
			Item: "manifest", Location: "<set>/manifest.json", Source: "wardynd -recovery-manifest",
			SetPath: "manifest.json",
			Note:    "written once with mode 0600; never overwritten",
		},
	}
}

func recordingsItem(f *bootFlags, where map[string]string) recoveryItem {
	it := recoveryItem{Item: "recordings", Source: "WARDYN_RECORDING_STORE=" + *f.recordingSel}
	switch *f.recordingSel {
	case "fs":
		it.Location = where["recordings"] + "; directory " + *f.recordingDir
		it.SetPath = "recordings/"
		it.Note = "the fs store's directory tree"
	case "pg":
		it.Location = "in the database (recording store pg)"
		it.Note = "collected with the database; no set path"
	default:
		it.Location = "none (recording store " + *f.recordingSel + ")"
		it.Note = "no recordings to collect"
	}
	return it
}

// keySources names the settings that hold key material, by name only.
func keySources(f *bootFlags) string {
	names := []string{"WARDYN_AGE_KEY"}
	if strings.TrimSpace(*f.platformKeyFile) != "" {
		names = append(names, "WARDYN_PLATFORM_KEY_FILE")
	}
	if kek := *f.vault.kek; kek != "" && kek != "local" {
		names = append(names, "WARDYN_KEK="+kek)
	}
	return strings.Join(names, ", ")
}

// dsnDisplay is the host, port and database of a DSN, which is all the set and the inventory may name.
// A DSN that does not parse is reported as such, and its error text is never printed: a parse error can
// echo the DSN.
func dsnDisplay(dsn string) string {
	if strings.TrimSpace(dsn) == "" {
		return "WARDYN_PG_DSN is unset"
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "WARDYN_PG_DSN does not parse"
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))) + "/" + cfg.Database
}
