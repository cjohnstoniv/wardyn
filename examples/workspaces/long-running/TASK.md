# Scenario 6 — Long-running: lifecycle reaper auto-stop

## Governance control exercised

Lifecycle reaper: `auto_stop_after_sec` in the run policy caps how long a
sandbox may run before wardynd forcibly stops it.  On expiry the reaper:
1. Calls StopSandbox (container removed, process tree killed).
2. Marks the run STOPPED.
3. Emits a `run.autostop` audit event with actor_type=system.

This demonstrates that even a misbehaving or stuck agent cannot run forever —
the operator sets a hard time limit at policy level.

## Prerequisite: a policy with auto_stop_after_sec set

The demo.json and claude-llm.json policies ship with `auto_stop_after_sec: 0`
(disabled).  For this scenario you need a policy that sets a finite value.
Create one with a 120-second limit (enough time to observe the behavior):

    cat > /tmp/reaper-policy.json << 'EOF'
    {
      "allowed_domains": ["github.com", "*.githubusercontent.com"],
      "denied_domains": [],
      "first_use_approval": "always_deny",
      "allowed_methods": [],
      "min_confinement_class": "CC1",
      "eligible_grants": [],
      "auto_stop_after_sec": 120
    }
    EOF

Upload and capture the policy ID:

    POLICY_ID=$(wardyn policy create --file /tmp/reaper-policy.json | jq -r .id)
    echo "Policy ID: $POLICY_ID"

## Exact task text (pass verbatim to the agent)

    Run python3 idle.py and report each tick as it prints.
    The script runs for approximately 10 minutes.

## Wardyn run command

    wardyn run \
      --agent claude-code \
      --repo your-org/long-running \
      --policy-id "$POLICY_ID" \
      --task "Run python3 idle.py and report each tick as it prints. The script runs for approximately 10 minutes."

## What to watch

- UI > Runs tab: run is RUNNING while the agent executes idle.py.
- After approximately 120 seconds (auto_stop_after_sec): run transitions to
  STOPPED and the container disappears from `docker ps`.
- UI > Audit tab: `run.autostop` event with actor_type=system.

CLI equivalents:

    # watch run state
    wardyn runs

    # confirm container removed
    docker ps | grep wardyn-agent

    # audit
    wardyn audit --run <run-id>
    # expect: action=run.autostop actor_type=system

## PASS criteria

1. Run reaches RUNNING state with the agent printing idle.py ticks.
2. After auto_stop_after_sec (120 s) the run transitions to STOPPED without
   any human intervention.
3. Audit log contains `run.autostop` with actor_type=system.
4. `docker ps` shows no container for this run (sandbox removed).
5. The Replay tab in the UI shows the ticks that were captured before the stop.
