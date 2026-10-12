// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Keystore is where resident values live; production is package keystore.
type Keystore struct {
	Set    func(account, value string) error
	Get    func(account string) (string, error)
	Delete func(account string) error
}

// Resident is the runner half of runner_resident delivery (design 0.9 §7.4).
// Values live only in the keystore and in the sandbox or proxy they are
// applied to. The index of what this runner holds (never a value) is kept in
// the state directory, so a restarted runner can still erase at run end.
type Resident struct {
	keystore Keystore
	index    string

	mu   sync.Mutex
	held map[uuid.UUID]map[uuid.UUID]residentGrant
}

type residentGrant struct {
	Sandbox  bool      `json:"sandbox,omitempty"`
	NotAfter time.Time `json:"not_after"`
}

// NewResident loads the index at stateDir/resident.json.
func NewResident(ks Keystore, stateDir string) (*Resident, error) {
	r := &Resident{keystore: ks, index: filepath.Join(stateDir, "resident.json"), held: map[uuid.UUID]map[uuid.UUID]residentGrant{}}
	b, err := os.ReadFile(r.index)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	return r, json.Unmarshal(b, &r.held)
}

func residentAccount(run, grant uuid.UUID) string { return run.String() + "/" + grant.String() }

// saveLocked rewrites the index; the caller holds mu.
func (r *Resident) saveLocked() error {
	b, err := json.Marshal(r.held)
	if err != nil {
		return err
	}
	tmp := r.index + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.index)
}

// keystoreTimeout bounds one keystore call: an OS prompt nobody answers must
// not hold a delivery or an erase open.
var keystoreTimeout = 30 * time.Second

// bounded runs one keystore call under keystoreTimeout. On a timeout the call
// may still finish later; callers keep the grant indexed so erase covers it.
func bounded[T any](ctx context.Context, f func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, keystoreTimeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() { v, err := f(); done <- result{v, err} }()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func keystoreRefusal(err error) error {
	return runnerwire.Refuse("%s: %v", placement.ReasonRunnerKeystoreUnavailable, err)
}

// Call serves deliver_resident and erase_resident.
func (r *Resident) Call(ctx context.Context, peer *runnerwire.Peer, method string, args json.RawMessage) (any, error) {
	if method == runnerwire.MethodEraseResident {
		var a runnerwire.EraseResidentArgs
		if err := runnerwire.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		return nil, r.erase(ctx, peer, a.RunID)
	}
	var a runnerwire.DeliverResidentArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.RunID == uuid.Nil || a.GrantID == uuid.Nil || a.Value == "" {
		return nil, runnerwire.Refuse("a resident delivery names a run, a grant and a value")
	}
	// Indexed before the write, so a write that times out and lands later is
	// still erased at run end.
	if err := r.track(a.RunID, a.GrantID, &residentGrant{Sandbox: a.Sandbox, NotAfter: a.NotAfter}); err != nil {
		return nil, err
	}
	account := residentAccount(a.RunID, a.GrantID)
	_, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, r.keystore.Set(account, a.Value) })
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return nil, keystoreRefusal(err)
	case err != nil:
		// No file fallback: a keystore that cannot hold the value refuses the run.
		return nil, errors.Join(keystoreRefusal(err), r.track(a.RunID, a.GrantID, nil))
	}
	return nil, nil
}

// track records (g non-nil) or forgets one grant of a run and saves the index.
func (r *Resident) track(run, grant uuid.UUID, g *residentGrant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case g != nil && r.held[run] == nil:
		r.held[run] = map[uuid.UUID]residentGrant{grant: *g}
	case g != nil:
		r.held[run][grant] = *g
	default:
		delete(r.held[run], grant)
		if len(r.held[run]) == 0 {
			delete(r.held, run)
		}
	}
	return r.saveLocked()
}

// erase deletes every value held for runID, best effort, and reports each
// grant's outcome as a resident_erase event. A grant whose delete failed stays
// indexed, so the next erase retries it.
func (r *Resident) erase(ctx context.Context, peer *runnerwire.Peer, runID uuid.UUID) error {
	r.mu.Lock()
	grants := slices.SortedFunc(maps.Keys(r.held[runID]), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	r.mu.Unlock()
	var errs []error
	for _, g := range grants {
		account := residentAccount(runID, g)
		outcome := runnerwire.ResidentErased
		if _, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, r.keystore.Delete(account) }); err != nil {
			outcome = runnerwire.ResidentEraseFailed
		} else {
			errs = append(errs, r.track(runID, g, nil))
		}
		data, err := runnerwire.Marshal(runnerwire.ResidentErase{GrantID: g, Outcome: outcome})
		if err == nil {
			err = peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventResidentErase, RunID: &runID, Data: data})
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// EraseExpired deletes every held value whose NotAfter has passed by now, whether or not its run's end
// ever reached this runner (an org erase that never arrived, a run the org lost). A value whose delete
// fails stays indexed, so the next sweep retries it.
func (r *Resident) EraseExpired(ctx context.Context, now time.Time) error {
	type held struct{ run, grant uuid.UUID }
	var expired []held
	r.mu.Lock()
	for run, grants := range r.held {
		for grant, g := range grants {
			if !g.NotAfter.IsZero() && now.After(g.NotAfter) {
				expired = append(expired, held{run, grant})
			}
		}
	}
	r.mu.Unlock()
	var errs []error
	for _, h := range expired {
		if _, err := bounded(ctx, func() (struct{}, error) { return struct{}{}, r.keystore.Delete(residentAccount(h.run, h.grant)) }); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, r.track(h.run, h.grant, nil))
	}
	return errors.Join(errs...)
}

// SweepExpired runs EraseExpired every interval until ctx ends.
func (r *Resident) SweepExpired(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.EraseExpired(ctx, time.Now()); err != nil {
			slog.WarnContext(ctx, "runner resident expiry sweep will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// residentToken matches a whole JSON string that is a placeholder.
var residentToken = regexp.MustCompile(`"` + regexp.QuoteMeta(strings.TrimSuffix(placement.ResidentPlaceholder(uuid.Nil), uuid.Nil.String())) + `([0-9a-f-]{36})"`)

// value reads one delivered value of the run; sandbox says it is going into
// the sandbox, which only a grant delivered for the sandbox may.
func (r *Resident) value(ctx context.Context, run, grant uuid.UUID, sandbox bool) (string, error) {
	if r == nil {
		return "", runnerwire.Refuse("runner credential delivery is unavailable")
	}
	r.mu.Lock()
	g, ok := r.held[run][grant]
	r.mu.Unlock()
	switch {
	case !ok:
		return "", runnerwire.Refuse("resident grant %s was not delivered to this runner for the run", grant)
	case sandbox && !g.Sandbox:
		return "", runnerwire.Refuse("resident grant %s is for the runner's proxy only, never the sandbox", grant)
	}
	v, err := bounded(ctx, func() (string, error) { return r.keystore.Get(residentAccount(run, grant)) })
	if err != nil {
		return "", keystoreRefusal(err)
	}
	return v, nil
}

// fillJSON replaces every placeholder string in one JSON value.
func (r *Resident) fillJSON(ctx context.Context, run uuid.UUID, raw []byte) ([]byte, error) {
	var err error
	out := residentToken.ReplaceAllFunc(raw, func(m []byte) []byte {
		grant, ok := placement.ResidentGrantID(string(bytes.Trim(m, `"`)))
		if !ok || err != nil {
			err = errors.Join(err, runnerwire.Refuse("malformed resident placeholder"))
			return m
		}
		v, verr := r.value(ctx, run, grant, false)
		if verr != nil {
			err = verr
			return m
		}
		enc, _ := json.Marshal(v)
		return enc
	})
	return out, err
}

// fillProxyFields fills placeholders in a proxy configuration object, only
// under its credential-bearing fields (placement.CredentialProxyField, by the
// Go field name fieldOf maps each key to). A placeholder anywhere else, such
// as a detail string shown to the sandbox, stays literal.
func (r *Resident) fillProxyFields(ctx context.Context, run uuid.UUID, cfg []byte, fieldOf func(string) string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &obj); err != nil {
		return nil, err
	}
	filled := false
	for key, raw := range obj {
		if !residentToken.Match(raw) || !placement.CredentialProxyField(fieldOf(key)) {
			continue
		}
		v, err := r.fillJSON(ctx, run, raw)
		if err != nil {
			return nil, err
		}
		obj[key], filled = v, true
	}
	if !filled {
		return cfg, nil
	}
	return json.Marshal(obj)
}

// fill is the spec CreateSandbox gets: each placeholder in SecretEnv,
// ManagedFiles or a credential field of the proxy configuration replaced by
// its delivered value. The org's spec, and every copy the runner persists,
// keeps the placeholders.
func (r *Resident) fill(ctx context.Context, spec runner.SandboxSpec) (runner.SandboxSpec, error) {
	env := maps.Clone(spec.SecretEnv)
	for k, v := range env {
		if grant, ok := placement.ResidentGrantID(v); ok {
			val, err := r.value(ctx, spec.RunID, grant, true)
			if err != nil {
				return runner.SandboxSpec{}, err
			}
			env[k] = val
		}
	}
	files := slices.Clone(spec.ManagedFiles)
	for i, f := range files {
		if grant, ok := placement.ResidentGrantID(string(f.Content)); ok {
			val, err := r.value(ctx, spec.RunID, grant, true)
			if err != nil {
				return runner.SandboxSpec{}, err
			}
			files[i].Content = []byte(val)
		}
	}
	spec.SecretEnv, spec.ManagedFiles = env, files
	pc, err := json.Marshal(spec.ProxyConfig)
	if err != nil || !residentToken.Match(pc) {
		return spec, err
	}
	// runner.ProxyConfig has no JSON tags: a key is its Go field name.
	if pc, err = r.fillProxyFields(ctx, spec.RunID, pc, func(key string) string { return key }); err != nil {
		return runner.SandboxSpec{}, err
	}
	spec.ProxyConfig = runner.ProxyConfig{}
	return spec, json.Unmarshal(pc, &spec.ProxyConfig)
}

// proxyConfigFields maps each proxy.Config JSON key to its Go field name.
var proxyConfigFields = func() map[string]string {
	t := reflect.TypeFor[proxy.Config]()
	out := make(map[string]string, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		out[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Name
	}
	return out
}()

// fillReplacedProxy re-applies resident values to the org's stripped stored
// proxy configuration on revive (ReplaceProxy).
func (r *Resident) fillReplacedProxy(ctx context.Context, cfg []byte) ([]byte, error) {
	if !residentToken.Match(cfg) {
		return cfg, nil
	}
	var head struct {
		RunID uuid.UUID `json:"run_id"`
	}
	if err := json.Unmarshal(cfg, &head); err != nil || head.RunID == uuid.Nil {
		return nil, runnerwire.Refuse("a proxy configuration with resident placeholders names no run")
	}
	return r.fillProxyFields(ctx, head.RunID, cfg, func(key string) string { return proxyConfigFields[key] })
}
