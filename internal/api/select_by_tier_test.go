// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tierStore answers the two reads each stale-snapshot entrance makes, the SAME
// way for the ceiling and the drive, so one case table can drive both.
type tierStore struct {
	noGovernanceStore
	tier       types.CapabilitySubjectType // "" = no row matched
	resolveErr error
	hasGroup   bool
	hasErr     error
	hasReads   int
	sawGroups  [][]string
}

func (s *tierStore) answer(groups []string) (types.CapabilitySubjectType, error) {
	s.sawGroups = append(s.sawGroups, groups)
	if s.resolveErr != nil {
		return "", s.resolveErr
	}
	if s.tier == "" {
		return "", store.ErrNotFound
	}
	return s.tier, nil
}

func (s *tierStore) ResolveGovernanceProfile(_ context.Context, _, groups []string, _ string) (
	*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	tier, err := s.answer(groups)
	if err != nil {
		return nil, "", err
	}
	return &types.GovernanceProfile{Name: "p-" + string(tier)}, tier, nil
}

func (s *tierStore) ResolveUserDrive(_ context.Context, _, groups []string, _ string) (
	*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error) {
	tier, err := s.answer(groups)
	if err != nil {
		return nil, nil, "", err
	}
	d := driveFixture(nil)
	return d, grantFixture(d.ID, func(g *types.UserDriveGrant) { g.SubjectType = tier }), tier, nil
}

func (s *tierStore) has() (bool, error) {
	s.hasReads++
	return s.hasGroup, s.hasErr
}

func (s *tierStore) HasGroupTierAssignments(context.Context) (bool, error) { return s.has() }
func (s *tierStore) HasGroupTierDriveGrants(context.Context) (bool, error) { return s.has() }

// TestSelectByTierOneRuleTwoEntrances pins K3's claim that the ceiling and the
// drive share ONE stale-snapshot rule: every case runs through both entrances
// and must give the same outcome, differing only in the audit row's target.
//
// The outcomes are the rule, written as a table: with the group tier
// unreadable, only a USER-tier winner or a deployment with no group-tier rows
// may be served; every other shape refuses groups_snapshot_stale (recorded once,
// except on a display read), and either read failing is an error, never "no
// row" and never the stale refusal.
func TestSelectByTierOneRuleTwoEntrances(t *testing.T) {
	boom := errors.New("pg down")
	type want struct {
		served  bool // a profile / a drive came back
		stale   bool
		err     bool // any other error
		hasRead bool // the group-tier read was made
		audited bool
	}
	cases := []struct {
		name    string
		st      tierStore
		display bool
		want    want
	}{
		{"user tier wins, group rows exist: served, group tier never read",
			tierStore{tier: types.CapabilitySubjectUser, hasGroup: true}, false,
			want{served: true}},
		{"user tier wins, group-tier read would fail: still served",
			tierStore{tier: types.CapabilitySubjectUser, hasErr: boom}, false,
			want{served: true}},
		{"user_type tier wins, group rows exist: refused",
			tierStore{tier: types.CapabilitySubjectUserType, hasGroup: true}, false,
			want{stale: true, hasRead: true, audited: true}},
		{"all tier wins, group rows exist: refused",
			tierStore{tier: types.CapabilitySubjectAll, hasGroup: true}, false,
			want{stale: true, hasRead: true, audited: true}},
		{"nothing matched, group rows exist: refused",
			tierStore{hasGroup: true}, false,
			want{stale: true, hasRead: true, audited: true}},
		{"refused on a display read: same answer, no row",
			tierStore{tier: types.CapabilitySubjectAll, hasGroup: true}, true,
			want{stale: true, hasRead: true}},
		{"user_type tier wins, no group rows: served",
			tierStore{tier: types.CapabilitySubjectUserType}, false,
			want{served: true, hasRead: true}},
		{"all tier wins, no group rows: served",
			tierStore{tier: types.CapabilitySubjectAll}, false,
			want{served: true, hasRead: true}},
		{"nothing matched, no group rows: the absent-row answer",
			tierStore{}, false,
			want{hasRead: true}},
		{"resolve fails: error, group tier never read",
			tierStore{resolveErr: boom, hasGroup: true}, false,
			want{err: true}},
		{"group-tier read fails after an all-tier winner: error, not served",
			tierStore{tier: types.CapabilitySubjectAll, hasErr: boom}, false,
			want{err: true, hasRead: true}},
		{"group-tier read fails after no match: error, not the absent-row answer",
			tierStore{hasErr: boom}, false,
			want{err: true, hasRead: true}},
	}
	entrances := []struct {
		target string
		call   func(ctx context.Context, srv *Server) (served bool, err error)
	}{
		{"governance.ceiling", func(ctx context.Context, srv *Server) (bool, error) {
			c, err := srv.ceilingWithUnusableGroups(ctx, []string{"sub-bob"}, "contractor", governanceCeiling{})
			return c.Profile != nil, err
		}},
		{"runs.drive", func(ctx context.Context, srv *Server) (bool, error) {
			d, err := srv.driveWithUnusableGroups(ctx, []string{"sub-bob"}, "contractor", driveSizeCeiling{})
			return d != nil, err
		}},
	}
	for _, tc := range cases {
		for _, e := range entrances {
			t.Run(e.target+"/"+tc.name, func(t *testing.T) {
				st := tc.st
				audit := &recRecorder{}
				srv := New(Config{Store: &st, Audit: audit, RunnerTarget: "docker"})
				ctx := context.Background()
				if tc.display {
					ctx = withDisplayRead(ctx)
				}
				served, err := e.call(ctx, srv)
				got := want{
					served:  served,
					stale:   errors.Is(err, errGroupsSnapshotStale),
					err:     err != nil && !errors.Is(err, errGroupsSnapshotStale),
					hasRead: st.hasReads > 0,
				}
				if len(st.sawGroups) != 1 || st.sawGroups[0] != nil {
					t.Errorf("resolved with groups %v, want exactly one read with none: an unusable snapshot "+
						"must not be matched against", st.sawGroups)
				}
				if tc.want.err && !errors.Is(err, boom) {
					t.Errorf("err = %v, want the store failure surfaced", err)
				}
				for _, ev := range audit.snapshot() {
					if ev.Action != "authz.denied" {
						continue
					}
					var data map[string]any
					if jerr := json.Unmarshal(ev.Data, &data); jerr != nil {
						t.Fatal(jerr)
					}
					if ev.Target != e.target || data["reason"] != "groups_snapshot_stale" {
						t.Errorf("denial row target=%s reason=%v, want %s groups_snapshot_stale",
							ev.Target, data["reason"], e.target)
					}
					if got.audited {
						t.Errorf("more than one denial row for one decision")
					}
					got.audited = true
				}
				if got != tc.want {
					t.Errorf("got %+v, want %+v (err=%v)", got, tc.want, err)
				}
			})
		}
	}
}
