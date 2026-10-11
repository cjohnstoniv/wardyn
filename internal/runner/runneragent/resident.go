// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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
	// No file fallback: a keystore that cannot hold the value refuses the run.
	if err := r.keystore.Set(residentAccount(a.RunID, a.GrantID), a.Value); err != nil {
		return nil, runnerwire.Refuse("%s: %v", placement.ReasonRunnerKeystoreUnavailable, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held[a.RunID] == nil {
		r.held[a.RunID] = map[uuid.UUID]residentGrant{}
	}
	r.held[a.RunID][a.GrantID] = residentGrant{Sandbox: a.Sandbox, NotAfter: a.NotAfter}
	if err := r.saveLocked(); err != nil {
		delete(r.held[a.RunID], a.GrantID)
		return nil, errors.Join(err, r.keystore.Delete(residentAccount(a.RunID, a.GrantID)))
	}
	return nil, nil
}

// erase deletes every value held for runID, best effort, and reports each
// grant's outcome as a resident_erase event.
func (r *Resident) erase(ctx context.Context, peer *runnerwire.Peer, runID uuid.UUID) error {
	r.mu.Lock()
	grants := slices.SortedFunc(maps.Keys(r.held[runID]), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	r.mu.Unlock()
	var errs []error
	for _, g := range grants {
		outcome := runnerwire.ResidentErased
		if err := r.keystore.Delete(residentAccount(runID, g)); err != nil {
			outcome = runnerwire.ResidentEraseFailed
		}
		data, err := runnerwire.Marshal(runnerwire.ResidentErase{GrantID: g, Outcome: outcome})
		if err == nil {
			err = peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventResidentErase, RunID: &runID, Data: data})
		}
		errs = append(errs, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, runID)
	return errors.Join(append(errs, r.saveLocked())...)
}

// residentToken matches a whole JSON string that is a placeholder.
var residentToken = regexp.MustCompile(`"` + regexp.QuoteMeta(strings.TrimSuffix(placement.ResidentPlaceholder(uuid.Nil), uuid.Nil.String())) + `([0-9a-f-]{36})"`)

// value reads one delivered value of the run; sandbox says it is going into
// the sandbox, which only a grant delivered for the sandbox may.
func (r *Resident) value(run, grant uuid.UUID, sandbox bool) (string, error) {
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
	v, err := r.keystore.Get(residentAccount(run, grant))
	if err != nil {
		return "", runnerwire.Refuse("%s: %v", placement.ReasonRunnerKeystoreUnavailable, err)
	}
	return v, nil
}

// fillProxyJSON replaces every placeholder string in a proxy configuration.
func (r *Resident) fillProxyJSON(run uuid.UUID, cfg []byte) ([]byte, error) {
	var err error
	out := residentToken.ReplaceAllFunc(cfg, func(m []byte) []byte {
		grant, ok := placement.ResidentGrantID(string(bytes.Trim(m, `"`)))
		if !ok || err != nil {
			err = errors.Join(err, runnerwire.Refuse("malformed resident placeholder"))
			return m
		}
		v, verr := r.value(run, grant, false)
		if verr != nil {
			err = verr
			return m
		}
		enc, _ := json.Marshal(v)
		return enc
	})
	return out, err
}

// fill is the spec CreateSandbox gets: each placeholder in SecretEnv,
// ManagedFiles or the proxy configuration replaced by its delivered value.
// The org's spec, and every copy the runner persists, keeps the placeholders.
func (r *Resident) fill(spec runner.SandboxSpec) (runner.SandboxSpec, error) {
	env := maps.Clone(spec.SecretEnv)
	for k, v := range env {
		if grant, ok := placement.ResidentGrantID(v); ok {
			val, err := r.value(spec.RunID, grant, true)
			if err != nil {
				return runner.SandboxSpec{}, err
			}
			env[k] = val
		}
	}
	files := slices.Clone(spec.ManagedFiles)
	for i, f := range files {
		if grant, ok := placement.ResidentGrantID(string(f.Content)); ok {
			val, err := r.value(spec.RunID, grant, true)
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
	if pc, err = r.fillProxyJSON(spec.RunID, pc); err != nil {
		return runner.SandboxSpec{}, err
	}
	spec.ProxyConfig = runner.ProxyConfig{}
	return spec, json.Unmarshal(pc, &spec.ProxyConfig)
}

// fillReplacedProxy re-applies resident values to the org's stripped stored
// proxy configuration on revive (ReplaceProxy).
func (r *Resident) fillReplacedProxy(cfg []byte) ([]byte, error) {
	if !residentToken.Match(cfg) {
		return cfg, nil
	}
	var head struct {
		RunID uuid.UUID `json:"run_id"`
	}
	if err := json.Unmarshal(cfg, &head); err != nil || head.RunID == uuid.Nil {
		return nil, runnerwire.Refuse("a proxy configuration with resident placeholders names no run")
	}
	return r.fillProxyJSON(head.RunID, cfg)
}
