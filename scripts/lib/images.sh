# scripts/lib/images.sh — shared docker-image predicate, sourced by the
# scripts that build-if-missing the per-run demo images (wardyn-proxy,
# agent-claude-code, agent-oracle, ...). Build commands, failure handling
# (die vs warn-and-continue) and log messages differ per caller on purpose —
# only the "is this image already built?" check is shared.

# image_missing IMAGE_REF — true (0) if IMAGE_REF is not present locally.
image_missing() { ! docker image inspect "$1" >/dev/null 2>&1; }
