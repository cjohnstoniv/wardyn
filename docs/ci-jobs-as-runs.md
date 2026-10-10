# CI jobs as confined one-shot runs

- Run each CI job as its own Wardyn run:
  - one sandbox per job, an image nobody can swap, egress limited to a list you wrote, the job's exit code becoming your pipeline's, and every job on the audit trail.
- This is a recipe over pieces that are documented separately, put in the order you need them. The worked example is a self-hosted Actions-compatible runner (Forgejo's) that registers, takes exactly one job and exits, all inside a confined run.
- This is not [CI.md](CI.md). That page is about running Wardyn *in* your pipeline ([`scripts/ci-run.sh`](../scripts/ci-run.sh), a throwaway control plane). This one is about running *your pipeline's jobs* on a Wardyn control plane you already operate.
- Exit codes, the CI principal and the `--policy-file` schema are defined there and in [POLICIES.md](POLICIES.md); this page does not repeat them.

## The pieces

| Piece | What it fixes | Who sets it | Where |
|---|---|---|---|
| Deployment default policy | The policy for every principal no profile binds, and the grant eligibility a profile can only narrow (which secret a grant may name) | Operator, at boot | `WARDYN_DEFAULT_POLICY` |
| Governance profile | The CI principal's ceiling: egress list, confinement floor, eligible grants; no interactive runs, no user drive, a cap on concurrent runs | Admin | `wardyn governance set` |
| Image capability | The one custom image the CI principal may launch | Admin | `/permissions` |
| Runner token | Stored by the CI principal, delivered to the run as an `env_secret` grant | CI principal | `wardyn secret set` |
| Run policy | This job's own egress list and grants, inside the ceiling | Your repo | `--policy-file` |
| Launch | `wardyn run --wait`: the job's exit code is the command's | Your launcher | `wardyn run` |

## Before you start

1. **A control plane with sign-in, and a CI principal that is not an admin.**
   - A profile binds a signed-in principal below `admin`; the admin token and local mode are exempt from every ceiling ([three-roles.md](operations/three-roles.md#three-roles-and-who-sets-the-walls)).
   - Give CI a dedicated person (role `user`) and mint that person's own `wdn_` token, as [CI.md](CI.md#cis-identity) describes.
   - Export it as `WARDYN_TOKEN` on the launcher.
   - `CI_USER` below is that person's email or sign-in subject.
2. **The CLI on the launcher**, with `WARDYN_URL` set to the control plane.
3. **The forge's address as `host:port`**, in `FORGE`.
   - A forge on a private address is refused by the proxy's private-address guard unless it is declared.
   - This recipe declares it the way the run policy below does, as an exact `host:port` entry in `allowed_domains`.
   - A forge reached by name on a private address needs `internal_hosts` instead ([OPERATIONS.md](OPERATIONS.md#upstream-proxy-the-bypass-list-upstream_proxy_no_proxy)).
4. **The runner image, by digest**, in `RUNNER_IMAGE`. A tag can move; a digest cannot.

- The commands below run in one of two identities, named on the first line of each block.
  - **admin** means `WARDYN_ADMIN_TOKEN` is the deployment's admin token.
  - **ci** means `WARDYN_TOKEN` is the CI principal's token and `WARDYN_ADMIN_TOKEN` is unset, because the CLI tries the admin token first and it would silently win.

## 1. Set the outer wall

- A profile can narrow the deployment's credential eligibility and never add to it.
- So the runner-token pairing has to be in the deployment default policy first.
- On a control plane dedicated to CI the default is also where every unassigned principal lands, so it stays sealed: no hosts, nothing waits for a human.

```sh
# admin (policy file read at wardynd boot; shown for reference)
cat > deployment-default.json <<'EOF'
{
  "allowed_domains": [],
  "first_use_approval": "always_deny",
  "min_confinement_class": "CC1",
  "eligible_grants": [
    {
      "kind": "env_secret",
      "scope": {"name": "RUNNER_TOKEN", "secret_name": "runner-registration-token"},
      "owner_only": true
    }
  ],
  "auto_stop_after_sec": 3600
}
EOF
```

- Point `WARDYN_DEFAULT_POLICY` at that file and restart wardynd.
- On the compose stack, save it as `policy.json` in a directory you mount with `WARDYN_MANAGED_DIR` (it appears read-only at `/etc/wardyn`) and set `WARDYN_DEFAULT_POLICY=/etc/wardyn/policy.json`.
- The Helm chart takes the same document as `defaultPolicy`.

> [!WARNING]
> An `env_secret` grant is the one grant kind a member cannot hold by default, because the value sits in the sandbox's environment for the whole run and every step of the job can read it.

- Turn it on for this deployment with `WARDYN_ALLOW_USER_ENV_SECRET=true` ([ENV.md](ENV.md)).
- The compose file does not pass that variable through, so add it with an override file:

```yaml
services:
  wardynd:
    environment:
      WARDYN_ALLOW_USER_ENV_SECRET: "true"
```

- Skip this and the token grant if the job needs no secret; a plain build job uses none.

## 2. Bound the CI principal with a governance profile

- The profile is the principal's ceiling.
- Whatever policy a job asks for is clamped to it, so a compromised launcher can request more and get less.

```sh
# admin
cat > ci-governance.json <<EOF
{
  "profiles": [
    {
      "id": "6f0f4f0e-2c6b-4b1e-9d53-0c1c1c1c1c1c",
      "name": "ci-jobs",
      "ceiling": {
        "allowed_domains": ["${FORGE}"],
        "first_use_approval": "always_deny",
        "min_confinement_class": "CC1",
        "eligible_grants": [
          {
            "kind": "env_secret",
            "scope": {"name": "RUNNER_TOKEN", "secret_name": "runner-registration-token"},
            "owner_only": true
          }
        ],
        "auto_stop_after_sec": 3600
      },
      "limits": {
        "deny_interactive": true,
        "deny_user_drive": true,
        "max_concurrent_runs": 4
      }
    }
  ],
  "assignments": [
    {
      "subject_type": "user",
      "subject": "${CI_USER}",
      "profile_id": "6f0f4f0e-2c6b-4b1e-9d53-0c1c1c1c1c1c",
      "priority": 0
    }
  ]
}
EOF
wardyn governance set ci-governance.json
```

- The `id` is any UUID you make up. It only ties the assignment to the profile inside this file.
- Assign at the `user` tier: a user-tier row settles the ceiling without depending on the token's group snapshot, which is frozen when the token is minted and fails closed when it is missing or truncated.
- `set` is an upsert by profile name, so re-running it changes nothing.

| Field | What it holds for CI |
|---|---|
| `allowed_domains` | Only the forge. A job's own policy can list fewer, never more. |
| `first_use_approval: always_deny` | An unlisted host is refused at once. Nothing waits for a reviewer nobody is watching. |
| `min_confinement_class` | The weakest sandbox a job may run in. A job asking for less is raised to it, and a launch the runner cannot satisfy is refused. Set it to the strongest class your substrate offers. |
| `deny_interactive` | Refuses an interactive run, including one that omits `--task`, which comes up interactive. It also refuses `wardyn run attach` and the SSH gateway into any run under the profile, the owner's exec runs included. |
| `deny_user_drive` | Refuses a user drive mount, the one storage that outlives a run. |
| `max_concurrent_runs` | A launch past the cap is refused. |

Both refusals are shown in [step 7](#7-check-the-refusals).

## 3. Pin the image

- Without a grant a member launches no custom image at all.
- Restricting one image to the people listed switches exactly that image on for them and leaves every other image off.
- The value is the rest of the path, so the slashes and `@` need no escaping.

```sh
# admin
curl -sS -X POST "$WARDYN_URL/api/v1/permissions/grants" \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"subject_type\":\"user\",\"subject\":\"${CI_USER}\",\"capability\":\"image\",\"value\":\"${RUNNER_IMAGE}\",\"effect\":\"allow\"}"
curl -sS -X PUT "$WARDYN_URL/api/v1/permissions/availability/image/${RUNNER_IMAGE}" \
  -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"restricted":true}'
```

- The second response lists the allow row under `allowed_by`.
- A launch of any other image, by tag or digest, is refused `403` and audited as `authz.denied` with reason `byoi_user`.

## 4. Store the runner token

- The CI principal stores its own row.
- `owner_only` in the grant means a run reads that row and never the operator's.
- The value is the registration token your forge issues (see the worked example below), and it comes from stdin because argv is readable in `ps`.

```sh
# ci
printf '%s' "$RUNNER_TOKEN_VALUE" | wardyn secret set runner-registration-token
wardyn secret list
```

- `secret list` shows `runner-registration-token (mine)`.
- There is no command that reads the value back.
- Rotate it by running `secret set` again.

## 5. Write the run policy

- The run policy is what your repo owns.
- It must fit inside the ceiling: anything outside is dropped with a warning at launch, not silently.

```sh
# ci
cat > ci-runner-policy.json <<EOF
{
  "allowed_domains": ["${FORGE}"],
  "first_use_approval": "always_deny",
  "min_confinement_class": "CC1",
  "eligible_grants": [
    {
      "kind": "env_secret",
      "scope": {"name": "RUNNER_TOKEN", "secret_name": "runner-registration-token"},
      "owner_only": true
    }
  ],
  "auto_stop_after_sec": 3600
}
EOF
wardyn policy render -f ci-runner-policy.json
```

- `policy render` rejects a misspelled field here, not at launch.
- Declare no `ui_apps`.
- Then check the whole launch without starting anything:

```sh
# ci
wardyn run --image "$RUNNER_IMAGE" --task-mode exec --task 'true' \
  --policy-file ci-runner-policy.json --dry-run
```

- It prints the confinement class that would be enforced and any setup blocker.

## 6. Launch one job and wait

- `--task-mode exec` runs the task as a plain shell command in your image, with no agent.
- `--wait` blocks until the run ends and exits with the outcome, so the launcher's status is the job's status.

```sh
# ci
wardyn run --image "$RUNNER_IMAGE" --task-mode exec \
  --task 'echo building; exit 7' \
  --policy-file ci-runner-policy.json --wait --timeout 2m --json >run.json
echo "exit $?"
```

- That prints `exit 7`: a `FAILED` run exits with the task's own code.
- The full table is in [CI.md](CI.md#exit-codes).
- The ones a launcher acts on:

| Exit | Meaning |
|---|---|
| `0` | The run `COMPLETED`. |
| the task's code | The run `FAILED`; this is what the job exited with. |
| `2` | The run was `KILLED` or `STOPPED`, or the launch was refused for who you are (an image not granted, an interactive run). |
| `3` | The launch was refused for what you asked (a policy or class the runner cannot meet, the run cap). |
| `124` | The wait timed out. **The run is still running.** |

- On `124`, kill the run. Otherwise a timeout leaves an untrusted job alive:

```sh
# ci
wardyn run --image "$RUNNER_IMAGE" --task-mode exec --task 'sleep 300' \
  --policy-file ci-runner-policy.json --wait --timeout 10s --json >slow.json
code=$?
[ "$code" -eq 124 ] && wardyn run kill "$(jq -r .id slow.json)"
echo "exit $code"
```

- `--json` puts the created run on stdout and progress on stderr, so `jq -r .id` is safe.
- The same launch from Go, without the token grant (it omits `eligible_grants`), with the exit code read the way the CLI reads it (from the `run.complete` event):

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func main() {
	ctx := context.Background()
	c := client.New(os.Getenv("WARDYN_URL"), os.Getenv("WARDYN_TOKEN"))
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Image:    os.Getenv("RUNNER_IMAGE"),
		TaskMode: "exec",
		Task:     "echo building; exit 7",
		InlinePolicy: &client.RunPolicySpec{
			AllowedDomains:      []string{os.Getenv("FORGE")},
			FirstUseApproval:    "always_deny",
			MinConfinementClass: "CC1",
			AutoStopAfterSec:    3600,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		run, err := c.GetRun(ctx, created.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if run.State.IsTerminal() {
			fmt.Println("state:", run.State)
			if run.State == client.RunCompleted {
				return
			}
			// The state commits just before run.complete is written, so look a few times.
			for i := 0; i < 5; i++ {
				events, _, _ := c.AuditEventsPage(ctx, created.ID, client.AuditFilter{ActionPrefix: "run.complete"})
				for _, e := range events {
					var d struct {
						ExitCode *int `json:"exit_code"`
					}
					if json.Unmarshal(e.Data, &d) == nil && d.ExitCode != nil {
						fmt.Println("exit code:", *d.ExitCode)
						os.Exit(*d.ExitCode)
					}
				}
				time.Sleep(time.Second)
			}
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
}
```

## 7. Check the refusals

- Two launches refused before any sandbox exists:

```sh
# ci
wardyn run --image ubuntu:24.04 --task-mode exec --task 'true' \
  --policy-file ci-runner-policy.json --dry-run
echo "exit $?"
wardyn run --image "$RUNNER_IMAGE" --policy-file ci-runner-policy.json --dry-run
echo "exit $?"
```

- The first is refused for the image, which step 3 did not grant (`403`, exit `2`).
- The second has no task, so it comes up interactive, and `ci-jobs` refuses it (`403`, exit `2`).

## 8. Read what the job did

- Every job leaves one trail, keyed by run id:

```sh
# ci
RUN_ID="$(jq -r .id run.json)"
wardyn audit "$RUN_ID"
```

| Action | What it records for a CI job |
|---|---|
| `run.create` | Who launched it, the task mode, the confinement class, whether the policy was inline, and any clamp warnings |
| `run.build` | The wrapped image and, as `byoi_base`, the exact image ref the job started from |
| `run.ceiling.reassert` | The governance profile that bounded it, by name |
| `run.env_secret.resolve` | The variable name, the secret name and whose row was read (`own`). Never the value |
| `run.policy.resolve` | The effective policy after clamping |
| `run.exec` | The command the sandbox ran |
| `egress.allow` / `egress.deny` | Every outbound decision: host, path, port, method and the `rule_source` that decided it. A repeated decision collapses into one row with a repeat count |
| `session.attach` / `session.detach` | Anyone who opened a terminal in the run |
| `run.complete` | The final state and exit code |

- Three queries a pipeline asks:

```sh
# ci
RUN_ID="$(jq -r .id run.json)"
wardyn audit "$RUN_ID" --json | jq -r '.[] | select(.action=="run.complete") | .data.exit_code'
wardyn audit "$RUN_ID" --json | jq -r '.[] | select(.action=="run.build") | .data.byoi_base'
wardyn audit "$RUN_ID" --outcome denied
```

- A launch that was refused never becomes a run, so it is not in `wardyn audit`.
- Those are deployment-level rows, and so are each launch's `secret.read` (purpose `dispatch`) and every profile write.
- An admin reads them:

```sh
# admin
curl -sS -H "Authorization: Bearer $WARDYN_ADMIN_TOKEN" "$WARDYN_URL/api/v1/audit?limit=200" |
  jq -r '.[] | select(.action == "authz.denied" or .action == "secret.read") | [.time, .action, .outcome] | @tsv'
```

## Worked example: a self-hosted Forgejo runner

- A Forgejo runner is a static binary with two commands that matter here: `register`, which trades a registration token for a runner identity, and `one-job`, which takes a single queued job, runs it and exits.
- That is a one-shot by construction, so the sandbox's life is the job's life.

- **On the forge**, once.
- Generate a registration token at the narrowest scope (here one repository, not the instance), and give the repository a workflow that targets the runner's label:

```sh
# forge host (Forgejo 11; the runner below is v11.3.1)
forgejo actions generate-runner-token --scope ciadmin/demo
```

```yaml
# .forgejo/workflows/ci.yml
name: ci
on: [push]
jobs:
  test:
    runs-on: self-hosted
    steps:
      - run: |
          echo "hello from a confined runner"
          echo "token vars in job env: $(env | grep -c RUNNER_TOKEN)"
          if wget -q -T 8 -O /dev/null https://example.com; then echo "example.com reachable"; else echo "example.com blocked"; fi
```

- Store the token as in step 4 (`RUNNER_TOKEN_VALUE`), and make it short-lived and used once: regenerate or revoke it on the forge after the run.
- Then the launcher runs this once per queued job (a webhook handler, a timer, another pipeline):

```sh
# ci
cat > runner-task.sh <<EOF
set -e
forgejo-runner register --no-interactive --instance "http://${FORGE}" \\
  --token "\$RUNNER_TOKEN" --name "wardyn-\$(cat /proc/sys/kernel/random/uuid)" \\
  --labels self-hosted:host
unset RUNNER_TOKEN
forgejo-runner one-job
EOF
wardyn run --image "$RUNNER_IMAGE" --task-mode exec --task "$(cat runner-task.sh)" \
  --policy-file ci-runner-policy.json --wait --timeout 30m --json >run.json
echo "exit $?"
```

- `RUNNER_IMAGE` is `code.forgejo.org/forgejo/runner@sha256:287433414b987b89896399683034818db198079053d84ca473aadb06ebad8b9f`, the upstream v11.3.1 image, used as is.
- The lab forge speaks plain `http`, which sends the registration token in cleartext; a real forge should use `https://` and `host:443`.

> [!WARNING]
> - **The token is readable by every step of the job, for the whole run.**
> - The grant puts the value in the sandbox container's environment. So the container's first process and the task's parent shell keep it (`/proc/1/environ`), and a job step can read and print it into the forge's job log.
> - `unset RUNNER_TOKEN` only stops plain inheritance: the workflow above prints `token vars in job env: 0` because `env` no longer shows it, which is not protection.
> - Treat the token as disclosed to every job the runner takes.
> - Scope it to one repository, make it short-lived and single use, and regenerate or revoke it once the run ends.

- The workflow's `wget` to `example.com` prints `example.com blocked`, and the run's audit trail has the matching `egress.deny` row, while the runner's own calls to the forge are `egress.allow`.
- The launcher exits `0`, since the job passed.

## What this recipe does not bound

- **UI apps, unless you add `deny_ui_apps`.**
  - The profile above does not set it, and an empty `ui_apps` in a ceiling is no opinion, so a job's own `ui_apps` survive the clamp.
  - Add `"deny_ui_apps": true` to the limits to strip them from every job and have the UI gateway refuse a session (#1391).
  - Without it, what holds is the run policy you author declaring none, and the UI-sandbox gateway staying off, which is its default ([UI-SANDBOXES.md](UI-SANDBOXES.md)).
- **The token is resident and readable.**
  - An `env_secret` is in the sandbox's environment for the whole run and every step of the job can read it.
  - `unset` does not change that, and no grant can take it back.
  - A registration token can register runners, so scope it to one repository, make it short-lived and single use, and regenerate or revoke it after the run.
  - Each run also leaves an offline runner row on the forge.
- **The forge.**
  - Wardyn bounds what the sandbox reaches.
  - What the forge does with a job, and who can push a workflow to it, is the forge's.

## Tested against

- Every command above ran, as written, against a throwaway compose install of this tree
  - (`wardynd` built from source, the `sso` profile with Dex as the identity provider, Docker as the runner so only the Fence class exists)
  - and a Forgejo 11 container, using Forgejo runner v11.3.1.
- The CI principal was a real signed-in `user` with its own token.
- The step 1 settings were applied to that install as described, and the workflow file was pushed through Forgejo's API rather than `git push`.
- The Helm chart route is named, not run.
