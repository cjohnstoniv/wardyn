// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Windows licence fence lives in the database: these tests insert and update agent_runs directly, as any
// create path would, and read what the triggers did. Each runs on its own database, because the switch is the
// site config singleton.

const winSwitchOn = `{"features":{"windows_vm":{"enabled":true}}}`

type winFix struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	pg   store.PG
}

func newWinFix(t *testing.T) winFix {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	f := winFix{t: t, ctx: context.Background(), pool: pool, pg: store.NewPG(pool)}
	f.setSwitch(winSwitchOn)
	return f
}

func (f winFix) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// setSwitch stores config as the site config; "" removes the row.
func (f winFix) setSwitch(config string) {
	f.t.Helper()
	f.exec(`DELETE FROM site_config`)
	if config != "" {
		f.exec(`INSERT INTO site_config (singleton, config) VALUES (true, $1::jsonb)`, config)
	}
}

func (f winFix) newPool(name string, seats int) types.WindowsLicencePool {
	f.t.Helper()
	p, err := f.pg.CreateWindowsLicencePool(f.ctx, types.WindowsLicencePool{Name: name, Seats: seats, CreatedBy: "admin"})
	if err != nil {
		f.t.Fatalf("create pool %s: %v", name, err)
	}
	return p
}

func (f winFix) newImage(name string, pool types.WindowsLicencePool) types.WindowsImage {
	f.t.Helper()
	ref := "registry.example/windows/" + strings.ToLower(strings.ReplaceAll(name, " ", "-")) + "@sha256:" + strings.Repeat(uuid.NewString()[:8], 8)
	img, err := f.pg.RegisterWindowsImage(f.ctx, types.WindowsImage{Name: name, Ref: ref, LicencePoolID: pool.ID, CreatedBy: "admin"})
	if err != nil {
		f.t.Fatalf("register image %s: %v", name, err)
	}
	return img
}

func (f winFix) addHolder(pool types.WindowsLicencePool, subject string) types.WindowsLicenceHolder {
	f.t.Helper()
	h, err := f.pg.AddWindowsLicenceHolder(f.ctx, pool.ID, subject, "admin")
	if err != nil {
		f.t.Fatalf("add holder %s: %v", subject, err)
	}
	return h
}

// winRun is a windows_vm run of img by subject in state, not yet inserted.
func winRun(img types.WindowsImage, subject string, state types.RunState) types.AgentRun {
	r := newRun(state)
	r.MachineKind = types.MachineWindowsVM
	r.Image = img.Ref
	r.CreatedBy = subject
	r.Interactive = true
	return r
}

// launch inserts a windows_vm run through CreateRun, the store's create path.
func (f winFix) launch(img types.WindowsImage, subject string) (types.AgentRun, error) {
	return f.pg.CreateRun(f.ctx, winRun(img, subject, types.RunPending))
}

func (f winFix) mustLaunch(img types.WindowsImage, subject string) types.AgentRun {
	f.t.Helper()
	r, err := f.launch(img, subject)
	if err != nil {
		f.t.Fatalf("launch by %s: %v", subject, err)
	}
	return r
}

func (f winFix) setState(run uuid.UUID, state types.RunState) error {
	_, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET state = $2, updated_at = now() WHERE id = $1`, run, string(state))
	return err
}

func (f winFix) leaseOf(run uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM windows_licence_leases WHERE run_id = $1`, run).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f winFix) leases() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM windows_licence_leases`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f winFix) runExists(run uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM agent_runs WHERE id = $1`, run).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

// refusedBy is the SQLSTATE and constraint a database error names, or "" for none.
func refusedBy(err error) string {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return ""
	}
	return pe.Code + " " + pe.ConstraintName
}

// setup is one pool of seats with one image, and the holders given.
func (f winFix) setup(seats int, holders ...string) (types.WindowsLicencePool, types.WindowsImage) {
	f.t.Helper()
	p := f.newPool("Engineering "+uuid.NewString()[:6], seats)
	img := f.newImage("Server "+uuid.NewString()[:6], p)
	for _, h := range holders {
		f.addHolder(p, h)
	}
	return p, img
}

func TestPG_WindowsLease_InsertWithoutHolderRefused(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(2)
	r := winRun(img, "ana@example.com", types.RunPending)
	if _, err := f.pg.CreateRun(f.ctx, r); !errors.Is(err, store.ErrWindowsLicenceNotHolder) {
		t.Fatalf("a launch in a pool with no holders = %v, want not a holder", err)
	}
	if f.runExists(r.ID) || f.leases() != 0 {
		t.Fatal("a refused launch left a run row or a seat")
	}
}

func TestPG_WindowsLease_NotHolder(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(2, "bo@example.com")
	f.setup(2, "ana@example.com") // Ana holds a seat, but in another pool
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicenceNotHolder) {
		t.Fatalf("a launch by a holder of another pool = %v, want not a holder", err)
	}
	// The creator is matched lower-cased and trimmed, as the holder row is written.
	r := f.mustLaunch(img, " Bo@Example.com")
	if !f.leaseOf(r.ID) {
		t.Fatal("the holder's launch took no seat")
	}
}

func TestPG_WindowsLease_PoolInactive(t *testing.T) {
	f := newWinFix(t)
	p, img := f.setup(2, "ana@example.com")
	if _, err := f.pg.DeleteWindowsLicencePool(f.ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicencePoolInactive) {
		t.Fatalf("a launch whose image's pool is deleted = %v, want pool inactive", err)
	}
	if _, err := f.pg.WindowsLaunchCheck(f.ctx, img.Ref, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicencePoolInactive) {
		t.Fatalf("the pre-check = %v, want pool inactive", err)
	}
}

func TestPG_WindowsLease_ImageUnregistered(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(2, "ana@example.com")
	stranger := img
	stranger.Ref = "registry.example/windows/other@sha256:" + strings.Repeat("0", 64)
	if _, err := f.launch(stranger, "ana@example.com"); !errors.Is(err, store.ErrWindowsImageUnregistered) {
		t.Fatalf("a launch of an unregistered ref = %v, want unregistered", err)
	}
	if _, err := f.pg.RetireWindowsImage(f.ctx, img.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsImageUnregistered) {
		t.Fatalf("a launch of a retired image = %v, want unregistered", err)
	}
	if _, err := f.pg.WindowsLaunchCheck(f.ctx, img.Ref, "ana@example.com"); !errors.Is(err, store.ErrWindowsImageUnregistered) {
		t.Fatalf("the pre-check = %v, want unregistered", err)
	}
	if f.leases() != 0 {
		t.Fatal("a refused launch took a seat")
	}
}

// winRunInsert is the plain INSERT any create path sends, inside tx-level control for the race tests.
const winRunInsert = `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, machine_kind, image)
	VALUES ($1, $2, 'claude-code', 'repo', 'CC1', 'PENDING', $3, 'docker', 'windows_vm', $4)`

func TestPG_WindowsLease_ConcurrentSameHolderOneWins(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(2, "ana@example.com")
	first, second := uuid.New(), uuid.New()
	tx1, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(f.ctx) }()
	if _, err = tx1.Exec(f.ctx, winRunInsert, first, "ana@example.com", "spiffe://t/"+first.String(), img.Ref); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(context.Background(), winRunInsert, second, "ana@example.com", "spiffe://t/"+second.String(), img.Ref)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the second launch finished before the first committed: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx1.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := refusedBy(<-done); got != "23514 windows_licence_in_use" {
		t.Fatalf("the second launch = %q, want 23514 windows_licence_in_use", got)
	}
	if !f.leaseOf(first) || f.runExists(second) || f.leases() != 1 {
		t.Fatal("want exactly the first launch holding the one seat")
	}
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicenceInUse) {
		t.Fatalf("a third launch through CreateRun = %v, want in use", err)
	}
	var inUse *store.WindowsLicenceInUseError
	if _, err := f.pg.WindowsLaunchCheck(f.ctx, img.Ref, "ana@example.com"); !errors.As(err, &inUse) || inUse.RunID != first {
		t.Fatalf("the pre-check = %v, want in use naming %s", err, first)
	}
}

func TestPG_WindowsHolders_AddBeyondSeatsRefused(t *testing.T) {
	f := newWinFix(t)
	p, _ := f.setup(2, "ana@example.com", "bo@example.com")
	if _, err := f.pg.AddWindowsLicenceHolder(f.ctx, p.ID, "cal@example.com", "admin"); !errors.Is(err, store.ErrWindowsLicencePoolFull) {
		t.Fatalf("a third holder of two seats = %v, want pool full", err)
	}
	if _, err := f.pg.AddWindowsLicenceHolder(f.ctx, p.ID, "ANA@example.com ", "admin"); err == nil {
		t.Fatal("a person took a second seat in one pool")
	}
	if hs, err := f.pg.ListWindowsLicenceHolders(f.ctx, p.ID); err != nil || len(hs) != 2 {
		t.Fatalf("holders = %v, %v; want the two", hs, err)
	}
}

func TestPG_WindowsHolders_SeatsBelowHoldersRefused(t *testing.T) {
	f := newWinFix(t)
	p, _ := f.setup(3, "ana@example.com", "bo@example.com")
	one, two := 1, 2
	if _, err := f.pg.UpdateWindowsLicencePool(f.ctx, p.ID, 0, nil, &one); !errors.Is(err, store.ErrWindowsLicenceSeatsBelowHolders) {
		t.Fatalf("one seat for two holders = %v, want refused", err)
	}
	got, err := f.pg.UpdateWindowsLicencePool(f.ctx, p.ID, 0, nil, &two)
	if err != nil || got.Seats != 2 {
		t.Fatalf("two seats for two holders = %+v, %v", got, err)
	}
	if _, err := f.pg.UpdateWindowsLicencePool(f.ctx, p.ID, p.Revision, nil, &two); !errors.Is(err, store.ErrWindowsLicenceStale) {
		t.Fatalf("a write at an old revision = %v, want stale", err)
	}
}

func TestPG_WindowsLease_TerminalReleases(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	for _, end := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived} {
		r := f.mustLaunch(img, "ana@example.com")
		for _, live := range []types.RunState{types.RunStarting, types.RunRunning, types.RunWaiting, types.RunRunning} {
			if err := f.setState(r.ID, live); err != nil || !f.leaseOf(r.ID) {
				t.Fatalf("%s: a move to %s = %v, or it freed the seat", end, live, err)
			}
		}
		if err := f.setState(r.ID, end); err != nil || f.leaseOf(r.ID) {
			t.Fatalf("a move to %s = %v, or it kept the seat", end, err)
		}
	}
}

func TestPG_WindowsLease_LostKeepsSeat(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	f.exec(`UPDATE agent_runs SET state = 'RUNNING', lost_at = now(), lost_reason = 'gone' WHERE id = $1`, r.ID)
	if !f.leaseOf(r.ID) {
		t.Fatal("a lost run freed its seat; its VM may still run")
	}
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicenceInUse) {
		t.Fatalf("a launch beside a lost run = %v, want in use", err)
	}
	if err := f.setState(r.ID, types.RunKilled); err != nil || f.leaseOf(r.ID) {
		t.Fatalf("killing the lost run = %v, or it kept the seat", err)
	}
	f.mustLaunch(img, "ana@example.com")
}

func TestPG_WindowsLease_AppRoleReadOnly(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	role := "wardyn_winapp_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	f.exec(`CREATE ROLE ` + role + ` NOLOGIN`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	// The app role is the role holding DELETE on run_attach_leases; the migration finds it from the catalog. Run the
	// licence migration again (it is re-runnable) so its grant step sees this role.
	f.exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON run_attach_leases, agent_runs TO ` + role)
	matches, err := filepath.Glob("../db/migrations/*_windows_licences.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("the licence migration: %v %v", matches, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	f.exec(string(body))
	for tbl, want := range map[string]string{
		"windows_licence_pools": "SELECT,INSERT,UPDATE,DELETE", "windows_licence_holders": "SELECT,INSERT,UPDATE,DELETE",
		"windows_images": "SELECT,INSERT,UPDATE,DELETE", "windows_licence_leases": "SELECT",
	} {
		var got []string
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
			var ok bool
			if err := f.pool.QueryRow(f.ctx, `SELECT has_table_privilege($1, $2, $3)`, role, tbl, priv).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, priv)
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: the app role holds %v, want %s", tbl, got, want)
		}
	}
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["role"] = role
	app, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	// The app role still launches and ends a Windows run: the triggers write the seat as their owner.
	run := uuid.New()
	if _, err := app.Exec(f.ctx, winRunInsert, run, "ana@example.com", "spiffe://t/"+run.String(), img.Ref); err != nil {
		t.Fatalf("the app role's launch: %v", err)
	}
	if !f.leaseOf(run) {
		t.Fatal("the app role's launch took no seat")
	}
	for _, sql := range []string{
		`DELETE FROM windows_licence_leases`,
		`UPDATE windows_licence_leases SET taken_at = now()`,
		`INSERT INTO windows_licence_leases (run_id, pool_id, holder_id, image_id) SELECT $1, pool_id, holder_id, image_id FROM windows_licence_leases`,
	} {
		var args []any
		if strings.Contains(sql, "$1") {
			args = []any{uuid.New()}
		}
		if _, err := app.Exec(f.ctx, sql, args...); refusedBy(err) != "42501 " {
			t.Errorf("the app role ran %q: %v, want permission denied", sql, err)
		}
	}
	if _, err := app.Exec(f.ctx, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1`, run); err != nil || f.leaseOf(run) {
		t.Fatalf("the app role's kill = %v, or the seat stayed", err)
	}
}

func TestPG_WindowsHolders_RemoveRefusedWhileLive(t *testing.T) {
	f := newWinFix(t)
	p, img := f.setup(2, "ana@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	var live *store.WindowsSeatLiveError
	if _, err := f.pg.RemoveWindowsLicenceHolder(f.ctx, p.ID, "Ana@example.com"); !errors.As(err, &live) || live.RunID != r.ID {
		t.Fatalf("removing a holder with a live seat = %v, want refused naming %s", err, r.ID)
	}
	if err := f.setState(r.ID, types.RunKilled); err != nil {
		t.Fatal(err)
	}
	if h, err := f.pg.RemoveWindowsLicenceHolder(f.ctx, p.ID, "ana@example.com"); err != nil || h.Subject != "ana@example.com" {
		t.Fatalf("removing the holder after the run ended = %+v, %v", h, err)
	}
	if _, err := f.launch(img, "ana@example.com"); !errors.Is(err, store.ErrWindowsLicenceNotHolder) {
		t.Fatalf("a launch after the seat was removed = %v, want not a holder", err)
	}
}

func TestPG_LeaverPurge_WindowsHolderKeptWhileLeaseLive(t *testing.T) {
	f := newWinFix(t)
	p, img := f.setup(2, "ana@example.com", "bo@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	var live *store.WindowsSeatLiveError
	if _, err := f.pg.DeleteUserSubjectRows(f.ctx, uuid.New(), []string{"ANA@example.com"}, nil); !errors.As(err, &live) || live.RunID != r.ID {
		t.Fatalf("a purge while the leaver's seat is live = %v, want refused naming %s", err, r.ID)
	}
	if hs, _ := f.pg.ListWindowsLicenceHolders(f.ctx, p.ID); len(hs) != 2 {
		t.Fatalf("holders after the refused purge = %v, want both kept", hs)
	}
	if err := f.setState(r.ID, types.RunKilled); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.DeleteUserSubjectRows(f.ctx, uuid.New(), []string{"ana@example.com"}, nil); err != nil {
		t.Fatalf("the retried purge: %v", err)
	}
	if hs, _ := f.pg.ListWindowsLicenceHolders(f.ctx, p.ID); len(hs) != 1 || hs[0].Subject != "bo@example.com" {
		t.Fatalf("holders after the purge = %v, want only Bo", hs)
	}
}

func TestPG_WindowsLease_SwitchOffRefusesInsert(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	for _, config := range []string{
		"", `{}`, `{"features":{}}`, `{"features":{"windows_vm":{}}}`, `{"features":{"windows_vm":{"enabled":null}}}`,
		`{"features":{"windows_vm":{"enabled":false}}}`, `{"features":{"windows_vm":{"enabled":"true"}}}`,
		`{"features":{"windows_vm":{"enabled":1}}}`, `{"features":{"windows_vm":true}}`,
	} {
		f.setSwitch(config)
		r := winRun(img, "ana@example.com", types.RunPending)
		if _, err := f.pg.CreateRun(f.ctx, r); !errors.Is(err, store.ErrWindowsVMsOff) {
			t.Errorf("switch %q: launch = %v, want off", config, err)
		}
		if f.runExists(r.ID) {
			t.Errorf("switch %q: the refused launch left a run row", config)
		}
	}
	f.setSwitch(winSwitchOn)
	if r := f.mustLaunch(img, "ana@example.com"); !f.leaseOf(r.ID) {
		t.Fatal("the switch on: the launch took no seat")
	}
	// Turning the switch off ends nothing: a live run keeps its seat and still ends normally.
	f.setSwitch(`{"features":{"windows_vm":{"enabled":false}}}`)
	if f.leases() != 1 {
		t.Fatal("turning the switch off freed a live seat")
	}
}

func TestPG_WindowsFence_KindChangeRefused(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	linux := newRun(types.RunRunning)
	linux.MachineKind = types.MachineLinuxVM
	linux.CreatedBy = "ana@example.com"
	linux.Image = img.Ref
	linux = persistRun(t, f.ctx, f.pool, linux)
	_, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET machine_kind = 'windows_vm' WHERE id = $1`, linux.ID)
	if got := refusedBy(err); got != "23514 windows_vm_kind_fixed" {
		t.Fatalf("linux_vm to windows_vm = %q, want windows_vm_kind_fixed", got)
	}
	win := f.mustLaunch(img, "ana@example.com")
	for _, kind := range []string{"linux_vm", "", "macos_vm"} {
		_, err = f.pool.Exec(f.ctx, `UPDATE agent_runs SET machine_kind = $2 WHERE id = $1`, win.ID, kind)
		if got := refusedBy(err); got != "23514 windows_vm_kind_fixed" {
			t.Errorf("windows_vm to %q = %q, want windows_vm_kind_fixed", kind, got)
		}
	}
	if !f.leaseOf(win.ID) {
		t.Fatal("a refused kind change freed the seat")
	}
}

func TestPG_WindowsFence_HolderChangeRefused(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(2, "ana@example.com", "bo@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	_, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET created_by = 'bo@example.com' WHERE id = $1`, r.ID)
	if got := refusedBy(err); got != "23514 windows_vm_holder_fixed" {
		t.Fatalf("a change of creator = %q, want windows_vm_holder_fixed", got)
	}
}

func TestPG_WindowsFence_ImageChangeRefused(t *testing.T) {
	f := newWinFix(t)
	p, img := f.setup(1, "ana@example.com")
	other := f.newImage("Other "+uuid.NewString()[:6], p)
	r := f.mustLaunch(img, "ana@example.com")
	_, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET image = $2 WHERE id = $1`, r.ID, other.Ref)
	if got := refusedBy(err); got != "23514 windows_vm_image_fixed" {
		t.Fatalf("a change of image = %q, want windows_vm_image_fixed", got)
	}
	err = f.pg.SetRunImage(f.ctx, r.ID, other.Ref)
	if !errors.Is(err, store.ErrWindowsVMFenced) || !strings.Contains(err.Error(), "windows_vm_image_fixed") {
		t.Fatalf("SetRunImage to another image = %v, want windows_vm_image_fixed", err)
	}
	if err := f.pg.SetRunImage(f.ctx, r.ID, img.Ref); err != nil {
		t.Fatalf("a provenance write of the same ref: %v", err)
	}
}

func TestPG_WindowsFence_EndedRunCannotReturn(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	for _, end := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived} {
		r := f.mustLaunch(img, "ana@example.com")
		if err := f.setState(r.ID, end); err != nil {
			t.Fatal(err)
		}
		for _, live := range []types.RunState{types.RunQueued, types.RunPending, types.RunStarting, types.RunRunning, types.RunWaiting} {
			if got := refusedBy(f.setState(r.ID, live)); got != "23514 windows_vm_ended" {
				t.Errorf("%s to %s = %q, want windows_vm_ended", end, live, got)
			}
		}
		other := types.RunArchived
		if end == types.RunArchived {
			other = types.RunKilled
		}
		if err := f.setState(r.ID, other); err != nil {
			t.Fatalf("%s to %s, another end state: %v", end, other, err)
		}
		if f.leaseOf(r.ID) {
			t.Fatalf("a run ended %s holds a seat", end)
		}
	}
}

func TestPG_WindowsFence_DeleteWithLiveSeatRefused(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	_, err := f.pool.Exec(f.ctx, `DELETE FROM agent_runs WHERE id = $1`, r.ID)
	if got := refusedBy(err); !strings.HasPrefix(got, "23503 ") {
		t.Fatalf("a delete of a run with a live seat = %q, want a foreign key refusal", got)
	}
	if !f.leaseOf(r.ID) {
		t.Fatal("the refused delete freed the seat")
	}
	if err := f.setState(r.ID, types.RunKilled); err != nil {
		t.Fatal(err)
	}
	f.exec(`DELETE FROM agent_runs WHERE id = $1`, r.ID)
}

func TestPG_WindowsFence_InsertedEndedRefused(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	for _, end := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived} {
		r := winRun(img, "ana@example.com", end)
		_, err := f.pg.CreateRun(f.ctx, r)
		if !errors.Is(err, store.ErrWindowsVMFenced) || !strings.Contains(err.Error(), "windows_vm_inserted_ended") {
			t.Errorf("a run inserted %s = %v, want windows_vm_inserted_ended", end, err)
		}
		if f.runExists(r.ID) || f.leases() != 0 {
			t.Fatalf("the refused %s insert left a run or a seat", end)
		}
	}
}

func TestPG_WindowsFence_FenceFiresWithoutColumnList(t *testing.T) {
	f := newWinFix(t)
	_, img := f.setup(1, "ana@example.com")
	r := f.mustLaunch(img, "ana@example.com")
	// A BEFORE trigger that sorts ahead of the fence rewrites the creator; the statement never names created_by.
	f.exec(`CREATE FUNCTION aa_test_rewrite_creator() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN NEW.created_by := 'mallory@example.com'; RETURN NEW; END $$`)
	f.exec(`CREATE TRIGGER aa_test_rewrite_creator BEFORE UPDATE ON agent_runs FOR EACH ROW EXECUTE FUNCTION aa_test_rewrite_creator()`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS aa_test_rewrite_creator ON agent_runs`)
	})
	_, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET updated_at = now() WHERE id = $1`, r.ID)
	if got := refusedBy(err); got != "23514 windows_vm_holder_fixed" {
		t.Fatalf("an update the earlier trigger rewrote = %q, want windows_vm_holder_fixed", got)
	}
}

// waitsThenRefused runs write while a launch of img holds its uncommitted seat: the write must wait for the
// launch to commit, then be refused naming the launched run.
func (f winFix) waitsThenRefused(img types.WindowsImage, name string, write func() error) {
	f.t.Helper()
	run := uuid.New()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, winRunInsert, run, "ana@example.com", "spiffe://t/"+run.String(), img.Ref); err != nil {
		f.t.Fatalf("%s: the launch: %v", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case err := <-done:
		f.t.Fatalf("%s finished before the launch committed: %v", name, err)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	var live *store.WindowsSeatLiveError
	if err := <-done; !errors.As(err, &live) || live.RunID != run {
		f.t.Fatalf("%s beside the new seat = %v, want refused naming %s", name, err, run)
	}
	if err := f.setState(run, types.RunKilled); err != nil {
		f.t.Fatal(err)
	}
}

func TestPG_WindowsFence_ImageRetireWaitsForLease(t *testing.T) {
	f := newWinFix(t)
	p, img := f.setup(1, "ana@example.com")
	other := f.newPool("Other "+uuid.NewString()[:6], 1)

	f.waitsThenRefused(img, "a pool move", func() error {
		_, err := f.pg.UpdateWindowsImage(f.ctx, img.ID, 0, nil, &other.ID)
		return err
	})
	if moved, err := f.pg.UpdateWindowsImage(f.ctx, img.ID, 0, nil, &other.ID); err != nil || moved.LicencePoolID != other.ID {
		t.Fatalf("the pool move once the run ended = %+v, %v", moved, err)
	}
	if _, err := f.pg.UpdateWindowsImage(f.ctx, img.ID, 0, nil, &p.ID); err != nil {
		t.Fatal(err)
	}

	f.waitsThenRefused(img, "a retire", func() error {
		_, err := f.pg.RetireWindowsImage(f.ctx, img.ID)
		return err
	})
	if got, err := f.pg.GetWindowsImage(f.ctx, img.ID); err != nil || got.State != types.WindowsImageActive {
		t.Fatalf("the refused retire left the image %+v, %v", got, err)
	}

	f.waitsThenRefused(img, "a pool delete", func() error {
		_, err := f.pg.DeleteWindowsLicencePool(f.ctx, p.ID)
		return err
	})
	if got, err := f.pg.DeleteWindowsLicencePool(f.ctx, p.ID); err != nil || got.State != types.WindowsLicencePoolDeleted {
		t.Fatalf("the pool delete once the run ended = %+v, %v", got, err)
	}
	if got, err := f.pg.RetireWindowsImage(f.ctx, img.ID); err != nil || got.State != types.WindowsImageRetired {
		t.Fatalf("the retire once the run ended = %+v, %v", got, err)
	}
}
