// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_pool_show.go — `runner pool show <pool>`: the pool, its runners, what they add up to, and what
// the pool itself narrows. The numbers come from the pool's members read, never from a second source.

func runnerPoolShowCmd(client clientFn) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <pool>", Short: "Show one pool you may use or administer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := readPoolArg(cmd, client, args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), p)
			}
			c := client()
			members, err := c.ListRunnerPoolMembers(cmd.Context(), p.ID)
			if err != nil {
				return err
			}
			me, who := "", ""
			if p.Scope == sdk.RunnerPoolUser {
				me = callerPrincipal(cmd.Context(), c)
			} else if text, ok, err := whoMayUse(cmd, c, p); err != nil {
				return err
			} else if ok {
				who = text
			}
			return printPool(cmd.OutOrStdout(), p, members, me, who)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit raw JSON")
	return cmd
}

// callerPrincipal is who the server says the caller is; "" when it does not say.
func callerPrincipal(ctx context.Context, c *sdk.Client) string {
	raw, err := c.Me(ctx)
	if err != nil {
		return ""
	}
	var me struct {
		Principal string `json:"principal"`
	}
	_ = json.Unmarshal(raw, &me)
	return me.Principal
}

// poolSums is what a pool's members add up to. Biggest is the largest per-run amount any member takes.
type poolSums struct {
	total, inUse, available sdk.RunnerTally
	biggest                 sdk.RunnerAmounts
}

func addTally(t *sdk.RunnerTally, o sdk.RunnerTally) {
	t.Runs += o.Runs
	t.CPUMillis += o.CPUMillis
	t.MemoryMiB += o.MemoryMiB
	t.DiskMiB += o.DiskMiB
}

// sumMembers adds up the members that declare limits; one without adds nothing. A pool that is not
// active can start nothing, so none of its capacity is available.
func sumMembers(p sdk.RunnerPool, members []sdk.RunnerPoolMemberView) poolSums {
	var s poolSums
	for _, m := range members {
		if m.Limits == nil {
			continue
		}
		addTally(&s.total, sdk.RunnerTally{Runs: m.Limits.RunsAtOnce, CPUMillis: m.Limits.Total.CPUMillis, MemoryMiB: m.Limits.Total.MemoryMiB, DiskMiB: m.Limits.Total.DiskMiB})
		addTally(&s.inUse, m.Usage.InUse)
		addTally(&s.available, m.Usage.Available)
		s.biggest.CPUMillis = max(s.biggest.CPUMillis, m.Limits.PerRun.CPUMillis)
		s.biggest.MemoryMiB = max(s.biggest.MemoryMiB, m.Limits.PerRun.MemoryMiB)
		s.biggest.DiskMiB = max(s.biggest.DiskMiB, m.Limits.PerRun.DiskMiB)
	}
	if p.State != sdk.RunnerPoolActive {
		s.available = sdk.RunnerTally{}
	}
	return s
}

// biggestRun is the biggest run one of the pool's runners can take, narrowed by the pool's caps: a run is
// sized by the one runner it starts on, so the amounts all come from the same runner. When no runner
// allows the most of everything, it is the one with the most CPU, then memory, then disk.
func biggestRun(members []sdk.RunnerPoolMemberView, caps sdk.RunnerAmounts) (out sdk.RunnerAmounts) {
	low := func(v, cap int64) int64 {
		if cap > 0 {
			return min(v, cap)
		}
		return v
	}
	for _, m := range members {
		if m.Limits == nil {
			continue
		}
		r := m.Limits.PerRun
		a := sdk.RunnerAmounts{CPUMillis: low(r.CPUMillis, caps.CPUMillis), MemoryMiB: low(r.MemoryMiB, caps.MemoryMiB), DiskMiB: low(r.DiskMiB, caps.DiskMiB)}
		if cmp.Or(cmp.Compare(a.CPUMillis, out.CPUMillis), cmp.Compare(a.MemoryMiB, out.MemoryMiB), cmp.Compare(a.DiskMiB, out.DiskMiB)) > 0 {
			out = a
		}
	}
	return out
}

// poolCaps is the pool's smallest cap on each resource across its run types; 0 is none.
func poolCaps(l *sdk.RunnerPoolLimits) (caps sdk.RunnerAmounts) {
	if l == nil {
		return caps
	}
	least := func(cur int64, a *sdk.RunnerPoolAmount) int64 {
		if a == nil || (cur != 0 && int64(a.Cap) >= cur) {
			return cur
		}
		return int64(a.Cap)
	}
	if b := l.Background; b != nil {
		caps = sdk.RunnerAmounts{CPUMillis: least(caps.CPUMillis, b.CPUMillis), MemoryMiB: least(caps.MemoryMiB, b.MemoryMiB), DiskMiB: least(caps.DiskMiB, b.DiskMiB)}
	}
	if i := l.Interactive; i != nil {
		caps = sdk.RunnerAmounts{CPUMillis: least(caps.CPUMillis, i.CPUMillis), MemoryMiB: least(caps.MemoryMiB, i.MemoryMiB), DiskMiB: least(caps.DiskMiB, i.DiskMiB)}
	}
	return caps
}

// poolLimitsText is the pool's own limits beside its runners': the resources it narrows below what they
// allow, the ones it sets without narrowing them (a maximum equal to or wider than theirs, whose default
// still applies), and its own runs maximum. It says the pool has none of its own only when it sets none.
func poolLimitsText(p sdk.RunnerPool, runners sdk.RunnerAmounts) string {
	var parts []string
	caps := poolCaps(p.Limits)
	var narrowed, set []string
	for _, r := range []struct {
		what         string
		cap, biggest int64
	}{{"cpu", caps.CPUMillis, runners.CPUMillis}, {"memory", caps.MemoryMiB, runners.MemoryMiB}, {"disk", caps.DiskMiB, runners.DiskMiB}} {
		switch {
		case r.cap > 0 && r.cap < r.biggest:
			narrowed = append(narrowed, r.what)
		case r.cap > 0:
			set = append(set, r.what)
		}
	}
	if len(narrowed) > 0 {
		parts = append(parts, strings.Join(narrowed, ", ")+" per run narrowed by this pool")
	}
	if len(set) > 0 {
		parts = append(parts, strings.Join(set, ", ")+" per run set by this pool")
	}
	if p.Limits != nil && p.Limits.MaxConcurrentRuns != nil {
		parts = append(parts, fmt.Sprintf("%d runs maximum in this pool", *p.Limits.MaxConcurrentRuns))
	}
	parts = append(parts, poolDurationsText(p.Limits)...)
	if len(parts) == 0 {
		return "none of its own: it follows its runners"
	}
	return strings.Join(parts, "; ")
}

// poolDurationsText is each lifetime and idle stop the pool sets, in the console's words: "background
// lifetime default 1 hour, up to 1 day". Its runners set none, so these are always the pool's own.
func poolDurationsText(l *sdk.RunnerPoolLimits) []string {
	if l == nil {
		return nil
	}
	var out []string
	say := func(what string, d *types.RunnerPoolDuration, open string) {
		if d == nil {
			return
		}
		switch {
		case !d.Unlimited:
			out = append(out, fmt.Sprintf("%s default %s, up to %s", what, durText(d.DefaultSec), durText(d.MaxSec)))
		case d.DefaultSec > 0:
			out = append(out, fmt.Sprintf("%s default %s, %s", what, durText(d.DefaultSec), open))
		default:
			out = append(out, what+" "+open)
		}
	}
	if b := l.Background; b != nil {
		say("background lifetime", b.Lifetime, "no maximum")
	}
	if i := l.Interactive; i != nil {
		say("interactive lifetime", i.Lifetime, "no maximum")
		say("interactive idle stop", i.Idle, "never")
	}
	return out
}

// durText is seconds as the console says them: "1 day", "2 hours", "30 minutes", "45 seconds".
func durText(sec int) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	switch {
	case sec >= 86400 && sec%86400 == 0:
		return unit(sec/86400, "day")
	case sec >= 3600 && sec%3600 == 0:
		return unit(sec/3600, "hour")
	case sec%60 == 0:
		return unit(sec/60, "minute")
	}
	return unit(sec, "second")
}

func poolRunTypes(l *sdk.RunnerPoolLimits) string {
	if l == nil {
		return "background, interactive"
	}
	var out []string
	if l.Background != nil {
		out = append(out, "background")
	}
	if l.Interactive != nil {
		out = append(out, "interactive")
	}
	return cmpNone(out)
}

func poolBarriers(l *sdk.RunnerPoolLimits) string {
	if l == nil || len(l.Barriers) == 0 {
		return "every barrier its runners offer"
	}
	out := make([]string, 0, len(l.Barriers))
	for _, b := range l.Barriers {
		name := types.ConfinementClassNames[b]
		if name == "" {
			name = string(b)
		}
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

func cmpNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// poolRunners is "name (self-hosted, online), ...", or "none yet".
func poolRunners(members []sdk.RunnerPoolMemberView) string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		kind, state := "remote-hosted", "offline"
		if m.Kind == sdk.RunnerKindSelfHosted {
			kind = "self-hosted"
		}
		if m.Online {
			state = "online"
		}
		out = append(out, fmt.Sprintf("%s (%s, %s)", m.Name, kind, state))
	}
	if len(out) == 0 {
		return "none yet"
	}
	return strings.Join(out, ", ")
}

func tallyText(t sdk.RunnerTally) string {
	return fmt.Sprintf("%s · runs %d", amountsText(sdk.RunnerAmounts{CPUMillis: t.CPUMillis, MemoryMiB: t.MemoryMiB, DiskMiB: t.DiskMiB}), t.Runs)
}

// printPool is the pool as `show` prints it. me is the caller's principal ("" when not known): the
// owner is marked "(you)" only when it is the caller. orgWho is an organization pool's WHO MAY USE row,
// "" when the caller may not read its use policy.
func printPool(w io.Writer, p sdk.RunnerPool, members []sdk.RunnerPoolMemberView, me, orgWho string) error {
	s := sumMembers(p, members)
	biggest := biggestRun(members, poolCaps(p.Limits))
	user := p.Scope == sdk.RunnerPoolUser
	rows := [][2]string{{"ID", p.ID.String()}, {"NAME", p.Name}, {"TYPE", map[bool]string{true: "user pool", false: "organization pool"}[user]}}
	who := "only " + p.Owner
	if user {
		owner := p.Owner
		if me != "" && p.Owner == me {
			owner, who = owner+" (you)", "only you"
		}
		rows = append(rows, [2]string{"OWNER", owner})
	}
	rows = append(rows, [2]string{"STATE", string(p.State)},
		[2]string{"RUN TYPES", poolRunTypes(p.Limits)},
		[2]string{"BARRIERS", poolBarriers(p.Limits)},
		[2]string{"RUNNERS", poolRunners(members)},
		[2]string{"TOTAL", tallyText(s.total) + "      (sum across its runners)"},
		[2]string{"IN USE", tallyText(s.inUse)},
		[2]string{"AVAILABLE", tallyText(s.available)},
		[2]string{"BIGGEST RUN", amountsText(biggest) + "             (the biggest run this pool can take)"},
		[2]string{"POOL LIMITS", poolLimitsText(p, s.biggest)})
	if !user {
		who = orgWho
	}
	if who != "" {
		rows = append(rows, [2]string{"WHO MAY USE", who})
	}
	tw := newTab(w)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1])
	}
	return tw.Flush()
}

func amountsText(a sdk.RunnerAmounts) string {
	return fmt.Sprintf("CPU %s · memory %s GB · disk %s GB", amount(a.CPUMillis, 1000), amount(a.MemoryMiB, 1024), amount(a.DiskMiB, 1024))
}
