# The go-cache-prune program: which Go cache entries a run's saves superseded (jq -rs, with --arg r "-<run id>" and
# --arg mod "<module-cache key or empty>"). Callers: ci.yml's go-cache-prune job, release-branch-checks.yml's, and scripts/test-ci-cache-prune.sh.
def fam: .key | sub("-[0-9]+$"; "") | sub("-[0-9a-f]{64}$"; "") | sub("-(Linux|Windows|macOS)$"; "");
(map(select((.key | endswith($r)) or ($mod != "" and .key == $mod))) | map({key: fam, value: .created_at}) | from_entries) as $fresh
| .[] | select(fam as $f | $fresh[$f] != null and .created_at < $fresh[$f])
| "\(.id)\t\(.key)"
