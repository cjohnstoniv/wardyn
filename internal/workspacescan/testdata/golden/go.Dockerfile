FROM mcr.microsoft.com/devcontainers/base:ubuntu

RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
      amd64) plat=linux-x64 ;; \
      arm64) plat=linux-arm64 ;; \
      *) echo "unsupported architecture for native claude-code install" >&2; exit 1 ;; \
    esac; \
    base=https://downloads.claude.ai/claude-code-releases; \
    ver="$(curl -fsSL --retry 20 --retry-all-errors --retry-max-time 300 --retry-delay 10 --connect-timeout 20 "$base/stable")"; \
    curl -fsSL --retry 20 --retry-all-errors --retry-max-time 300 --retry-delay 10 --connect-timeout 20 "$base/$ver/manifest.json" -o /tmp/claude-manifest.json; \
    sum="$(grep -A3 "\"$plat\"" /tmp/claude-manifest.json | grep -oiE '[a-f0-9]{64}' | head -1)"; \
    [ -n "$sum" ] || { echo "no sha256 for $plat in $ver manifest" >&2; exit 1; }; \
    curl -fsSL --retry 20 --retry-all-errors --retry-max-time 300 --retry-delay 10 --connect-timeout 20 "$base/$ver/$plat/claude" -o /usr/local/bin/claude; \
    echo "$sum  /usr/local/bin/claude" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/claude; \
    rm -f /tmp/claude-manifest.json
