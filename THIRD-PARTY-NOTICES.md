# Third-party notices

Wardyn is Apache-2.0. This file lists the third-party components distributed
with it and the licences they are distributed under. Verbatim licence texts are
in `licenses/texts/`. Regenerate with `make notices ARGS=fix`; CI fails on drift.

## Go modules (compiled into the shipped binaries)

Scope: reachable from `./cmd/...` under the production build tags `docker,k8s`.

| module | licence | source |
|---|---|---|
| `filippo.io/age` | BSD-3-Clause | https://github.com/FiloSottile/age/blob/v1.2.1/LICENSE |
| `github.com/bradleyfalzon/ghinstallation/v2` | Apache-2.0 | https://github.com/bradleyfalzon/ghinstallation/blob/v2.19.0/LICENSE |
| `github.com/cespare/xxhash/v2` | MIT | https://github.com/cespare/xxhash/blob/v2.3.0/LICENSE.txt |
| `github.com/coder/websocket` | ISC | https://github.com/coder/websocket/blob/v1.8.14/LICENSE.txt |
| `github.com/containerd/errdefs` | Apache-2.0 | https://github.com/containerd/errdefs/blob/v1.0.0/LICENSE |
| `github.com/containerd/errdefs/pkg` | Apache-2.0 | https://github.com/containerd/errdefs/blob/pkg/v0.3.0/pkg/LICENSE |
| `github.com/coreos/go-oidc/v3/oidc` | Apache-2.0 | https://github.com/coreos/go-oidc/blob/v3.20.0/LICENSE |
| `github.com/davecgh/go-spew/spew` | ISC | https://github.com/davecgh/go-spew/blob/d8f796af33cc/LICENSE |
| `github.com/distribution/reference` | Apache-2.0 | https://github.com/distribution/reference/blob/v0.6.0/LICENSE |
| `github.com/docker/go-connections` | Apache-2.0 | https://github.com/docker/go-connections/blob/v0.7.0/LICENSE |
| `github.com/docker/go-units` | Apache-2.0 | https://github.com/docker/go-units/blob/v0.5.0/LICENSE |
| `github.com/emicklei/go-restful/v3` | MIT | https://github.com/emicklei/go-restful/blob/v3.13.0/LICENSE |
| `github.com/felixge/httpsnoop` | MIT | https://github.com/felixge/httpsnoop/blob/v1.0.4/LICENSE.txt |
| `github.com/fxamacker/cbor/v2` | MIT | https://github.com/fxamacker/cbor/blob/v2.9.0/LICENSE |
| `github.com/go-chi/chi/v5` | MIT | https://github.com/go-chi/chi/blob/v5.2.4/LICENSE |
| `github.com/go-jose/go-jose/v4` | Apache-2.0 | https://github.com/go-jose/go-jose/blob/v4.1.4/LICENSE |
| `github.com/go-jose/go-jose/v4/json` | BSD-3-Clause | https://github.com/go-jose/go-jose/blob/v4.1.4/json/LICENSE |
| `github.com/go-logr/logr` | Apache-2.0 | https://github.com/go-logr/logr/blob/v1.4.3/LICENSE |
| `github.com/go-logr/stdr` | Apache-2.0 | https://github.com/go-logr/stdr/blob/v1.2.2/LICENSE |
| `github.com/go-openapi/jsonpointer` | Apache-2.0 | https://github.com/go-openapi/jsonpointer/blob/v0.21.0/LICENSE |
| `github.com/go-openapi/jsonreference` | Apache-2.0 | https://github.com/go-openapi/jsonreference/blob/v0.20.2/LICENSE |
| `github.com/go-openapi/swag` | Apache-2.0 | https://github.com/go-openapi/swag/blob/v0.23.0/LICENSE |
| `github.com/golang-jwt/jwt/v4` | MIT | https://github.com/golang-jwt/jwt/blob/v4.5.2/LICENSE |
| `github.com/google/gnostic-models` | Apache-2.0 | https://github.com/google/gnostic-models/blob/v0.7.0/LICENSE |
| `github.com/google/go-github/v88/github` | BSD-3-Clause | https://github.com/google/go-github/blob/v88.0.0/LICENSE |
| `github.com/google/go-querystring/query` | BSD-3-Clause | https://github.com/google/go-querystring/blob/v1.2.0/LICENSE |
| `github.com/google/uuid` | BSD-3-Clause | https://github.com/google/uuid/blob/v1.6.0/LICENSE |
| `github.com/gorilla/websocket` | BSD-2-Clause | https://github.com/gorilla/websocket/blob/e064f32e3674/LICENSE |
| `github.com/jackc/pgpassfile` | MIT | https://github.com/jackc/pgpassfile/blob/v1.0.0/LICENSE |
| `github.com/jackc/pgservicefile` | MIT | https://github.com/jackc/pgservicefile/blob/5a60cdf6a761/LICENSE |
| `github.com/jackc/pgx/v5` | MIT | https://github.com/jackc/pgx/blob/v5.10.0/LICENSE |
| `github.com/jackc/puddle/v2` | MIT | https://github.com/jackc/puddle/blob/v2.2.2/LICENSE |
| `github.com/josharian/intern` | MIT | https://github.com/josharian/intern/blob/v1.0.0/license.md |
| `github.com/json-iterator/go` | MIT | https://github.com/json-iterator/go/blob/v1.1.12/LICENSE |
| `github.com/mailru/easyjson` | MIT | https://github.com/mailru/easyjson/blob/v0.7.7/LICENSE |
| `github.com/moby/docker-image-spec/specs-go/v1` | Apache-2.0 | https://github.com/moby/docker-image-spec/blob/v1.3.1/LICENSE |
| `github.com/moby/moby/api` | Apache-2.0 | https://github.com/moby/moby/blob/api/v1.55.0/api/LICENSE |
| `github.com/moby/moby/client` | Apache-2.0 | https://github.com/moby/moby/blob/client/v0.5.0/client/LICENSE |
| `github.com/moby/spdystream` | Apache-2.0 | https://github.com/moby/spdystream/blob/v0.5.1/LICENSE |
| `github.com/moby/spdystream/spdy` | BSD-3-Clause | https://github.com/moby/spdystream/blob/v0.5.1/spdy/LICENSE |
| `github.com/modern-go/concurrent` | Apache-2.0 | https://github.com/modern-go/concurrent/blob/bacd9c7ef1dd/LICENSE |
| `github.com/modern-go/reflect2` | Apache-2.0 | https://github.com/modern-go/reflect2/blob/35a7c28c31ee/LICENSE |
| `github.com/munnerz/goautoneg` | BSD-3-Clause | https://github.com/munnerz/goautoneg/blob/a7dc8b61c822/LICENSE |
| `github.com/opencontainers/go-digest` | Apache-2.0 | https://github.com/opencontainers/go-digest/blob/v1.0.0/LICENSE |
| `github.com/opencontainers/image-spec/specs-go` | Apache-2.0 | https://github.com/opencontainers/image-spec/blob/v1.1.1/LICENSE |
| `github.com/spf13/cobra` | Apache-2.0 | https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt |
| `github.com/spf13/pflag` | BSD-3-Clause | https://github.com/spf13/pflag/blob/v1.0.9/LICENSE |
| `github.com/spiffe/go-spiffe/v2/spiffeid` | Apache-2.0 | https://github.com/spiffe/go-spiffe/blob/v2.6.0/LICENSE |
| `github.com/x448/float16` | MIT | https://github.com/x448/float16/blob/v0.8.4/LICENSE |
| `go.opentelemetry.io/auto/sdk` | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go-instrumentation/blob/sdk/v1.2.1/sdk/LICENSE |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go-contrib/blob/instrumentation/net/http/otelhttp/v0.69.0/instrumentation/net/http/otelhttp/LICENSE |
| `go.opentelemetry.io/otel` | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go/blob/v1.44.0/LICENSE |
| `go.opentelemetry.io/otel/metric` | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go/blob/metric/v1.44.0/metric/LICENSE |
| `go.opentelemetry.io/otel/trace` | Apache-2.0 | https://github.com/open-telemetry/opentelemetry-go/blob/trace/v1.44.0/trace/LICENSE |
| `go.yaml.in/yaml/v2` | Apache-2.0 | https://github.com/yaml/go-yaml/blob/v2.4.3/LICENSE |
| `go.yaml.in/yaml/v3` | MIT | https://github.com/yaml/go-yaml/blob/v3.0.4/LICENSE |
| `golang.org/x/crypto` | BSD-3-Clause | https://cs.opensource.google/go/x/crypto/+/v0.54.0:LICENSE |
| `golang.org/x/net` | BSD-3-Clause | https://cs.opensource.google/go/x/net/+/v0.56.0:LICENSE |
| `golang.org/x/oauth2` | BSD-3-Clause | https://cs.opensource.google/go/x/oauth2/+/v0.36.0:LICENSE |
| `golang.org/x/sync/semaphore` | BSD-3-Clause | https://cs.opensource.google/go/x/sync/+/v0.22.0:LICENSE |
| `golang.org/x/sys` | BSD-3-Clause | https://cs.opensource.google/go/x/sys/+/v0.47.0:LICENSE |
| `golang.org/x/term` | BSD-3-Clause | https://cs.opensource.google/go/x/term/+/v0.45.0:LICENSE |
| `golang.org/x/text` | BSD-3-Clause | https://cs.opensource.google/go/x/text/+/v0.40.0:LICENSE |
| `golang.org/x/time/rate` | BSD-3-Clause | https://cs.opensource.google/go/x/time/+/v0.14.0:LICENSE |
| `google.golang.org/protobuf` | BSD-3-Clause | https://github.com/protocolbuffers/protobuf-go/blob/f2248ac996af/LICENSE |
| `gopkg.in/evanphx/json-patch.v4` | BSD-3-Clause | https://github.com/evanphx/json-patch/blob/v4.13.0/LICENSE |
| `gopkg.in/inf.v0` | BSD-3-Clause | https://github.com/go-inf/inf/blob/v0.9.1/LICENSE |
| `gopkg.in/yaml.v3` | MIT | https://github.com/go-yaml/yaml/blob/v3.0.1/LICENSE |
| `k8s.io/api` | Apache-2.0 | https://github.com/kubernetes/api/blob/v0.36.3/LICENSE |
| `k8s.io/apimachinery/pkg` | Apache-2.0 | https://github.com/kubernetes/apimachinery/blob/v0.36.3/LICENSE |
| `k8s.io/apimachinery/third_party/forked/golang` | BSD-3-Clause | https://github.com/kubernetes/apimachinery/blob/v0.36.3/third_party/forked/golang/LICENSE |
| `k8s.io/client-go` | Apache-2.0 | https://github.com/kubernetes/client-go/blob/v0.36.3/LICENSE |
| `k8s.io/klog/v2` | Apache-2.0 | https://github.com/kubernetes/klog/blob/v2.140.0/LICENSE |
| `k8s.io/kube-openapi/pkg` | Apache-2.0 | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/LICENSE |
| `k8s.io/kube-openapi/pkg/internal/third_party/go-json-experiment/json` | BSD-3-Clause | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/internal/third_party/go-json-experiment/json/LICENSE |
| `k8s.io/kube-openapi/pkg/validation/spec` | Apache-2.0 | https://github.com/kubernetes/kube-openapi/blob/43fb72c5454a/pkg/validation/spec/LICENSE |
| `k8s.io/streaming/pkg` | Apache-2.0 | https://github.com/kubernetes/streaming/blob/v0.36.3/LICENSE |
| `k8s.io/utils` | Apache-2.0 | https://github.com/kubernetes/utils/blob/b8788abfbbc2/LICENSE |
| `k8s.io/utils/internal/third_party/forked/golang/net` | BSD-3-Clause | https://github.com/kubernetes/utils/blob/b8788abfbbc2/internal/third_party/forked/golang/LICENSE |
| `sigs.k8s.io/json` | Apache-2.0 | https://github.com/kubernetes-sigs/json/blob/2d320260d730/LICENSE |
| `sigs.k8s.io/randfill` | Apache-2.0 | https://github.com/kubernetes-sigs/randfill/blob/v1.0.0/LICENSE |
| `sigs.k8s.io/structured-merge-diff/v6` | Apache-2.0 | https://github.com/kubernetes-sigs/structured-merge-diff/blob/v6.3.3/LICENSE |
| `sigs.k8s.io/yaml` | Apache-2.0 | https://github.com/kubernetes-sigs/yaml/blob/v1.6.0/LICENSE |

## UI packages (bundled into the console shipped inside wardynd)

| package | version | licence |
|---|---|---|
| `@babel/runtime` | 7.29.7 | MIT |
| `@floating-ui/core` | 1.7.5 | MIT |
| `@floating-ui/dom` | 1.7.6 | MIT |
| `@floating-ui/react-dom` | 2.1.8 | MIT |
| `@floating-ui/utils` | 0.2.11 | MIT |
| `@fontsource/inter` | 5.2.8 | OFL-1.1 |
| `@fontsource/jetbrains-mono` | 5.2.8 | OFL-1.1 |
| `@radix-ui/number` | 1.1.0 | MIT |
| `@radix-ui/primitive` | 1.1.1 | MIT |
| `@radix-ui/react-alert-dialog` | 1.1.6 | MIT |
| `@radix-ui/react-arrow` | 1.1.2 | MIT |
| `@radix-ui/react-checkbox` | 1.1.4 | MIT |
| `@radix-ui/react-collection` | 1.1.2 | MIT |
| `@radix-ui/react-compose-refs` | 1.1.1, 1.1.3 | MIT |
| `@radix-ui/react-context` | 1.1.1 | MIT |
| `@radix-ui/react-dialog` | 1.1.6 | MIT |
| `@radix-ui/react-direction` | 1.1.0 | MIT |
| `@radix-ui/react-dismissable-layer` | 1.1.5 | MIT |
| `@radix-ui/react-dropdown-menu` | 2.1.6 | MIT |
| `@radix-ui/react-focus-guards` | 1.1.1 | MIT |
| `@radix-ui/react-focus-scope` | 1.1.2 | MIT |
| `@radix-ui/react-id` | 1.1.0, 1.1.2 | MIT |
| `@radix-ui/react-label` | 2.1.11 | MIT |
| `@radix-ui/react-menu` | 2.1.6 | MIT |
| `@radix-ui/react-popover` | 1.1.6 | MIT |
| `@radix-ui/react-popper` | 1.2.2 | MIT |
| `@radix-ui/react-portal` | 1.1.4 | MIT |
| `@radix-ui/react-presence` | 1.1.2 | MIT |
| `@radix-ui/react-primitive` | 2.0.2, 2.1.6, 2.1.7 | MIT |
| `@radix-ui/react-radio-group` | 1.2.3 | MIT |
| `@radix-ui/react-roving-focus` | 1.1.2 | MIT |
| `@radix-ui/react-select` | 2.1.6 | MIT |
| `@radix-ui/react-slot` | 1.1.2, 1.3.0 | MIT |
| `@radix-ui/react-tabs` | 1.1.3 | MIT |
| `@radix-ui/react-use-callback-ref` | 1.1.0 | MIT |
| `@radix-ui/react-use-controllable-state` | 1.1.0 | MIT |
| `@radix-ui/react-use-escape-keydown` | 1.1.0 | MIT |
| `@radix-ui/react-use-layout-effect` | 1.1.0, 1.1.2 | MIT |
| `@radix-ui/react-use-previous` | 1.1.0 | MIT |
| `@radix-ui/react-use-rect` | 1.1.0 | MIT |
| `@radix-ui/react-use-size` | 1.1.0 | MIT |
| `@radix-ui/react-visually-hidden` | 1.1.2 | MIT |
| `@radix-ui/rect` | 1.1.0 | MIT |
| `@solid-primitives/refs` | 1.1.3 | MIT |
| `@solid-primitives/transition-group` | 1.1.2 | MIT |
| `@solid-primitives/utils` | 6.4.0 | MIT |
| `@types/prop-types` | 15.7.15 | MIT |
| `@types/react` | 18.3.12 | MIT |
| `@types/react-dom` | 18.3.1 | MIT |
| `@xterm/addon-fit` | 0.11.0 | MIT |
| `@xterm/xterm` | 6.0.0 | MIT |
| `aria-hidden` | 1.2.6 | MIT |
| `asciinema-player` | 3.16.0 | Apache-2.0 |
| `class-variance-authority` | 0.7.1 | Apache-2.0 |
| `clsx` | 2.1.1 | MIT |
| `cmdk` | 1.1.1 | MIT |
| `cookie` | 1.1.1 | MIT |
| `csstype` | 3.2.3 | MIT |
| `detect-node-es` | 1.1.0 | MIT |
| `fast-equals` | 4.0.3 | MIT |
| `get-nonce` | 1.0.1 | MIT |
| `js-tokens` | 4.0.0 | MIT |
| `loose-envify` | 1.4.0 | MIT |
| `lucide-react` | 1.24.0 | ISC |
| `object-assign` | 4.1.1 | MIT |
| `prop-types` | 15.8.1 | MIT |
| `react` | 18.3.1 | MIT |
| `react-dom` | 18.3.1 | MIT |
| `react-draggable` | 4.7.1 | MIT |
| `react-grid-layout` | 2.2.4 | MIT |
| `react-is` | 16.13.1 | MIT |
| `react-remove-scroll` | 2.7.2 | MIT |
| `react-remove-scroll-bar` | 2.3.8 | MIT |
| `react-resizable` | 3.2.0 | MIT |
| `react-router` | 7.18.2 | MIT |
| `react-router-dom` | 7.18.2 | MIT |
| `react-style-singleton` | 2.2.3 | MIT |
| `resize-observer-polyfill` | 1.5.1 | MIT |
| `scheduler` | 0.23.2 | MIT |
| `seroval` | 1.5.4 | MIT |
| `seroval-plugins` | 1.5.4 | MIT |
| `set-cookie-parser` | 2.7.2 | MIT |
| `solid-js` | 1.9.13 | MIT |
| `solid-transition-group` | 0.2.3 | MIT |
| `sonner` | 2.0.3 | MIT |
| `tailwind-merge` | 3.2.0 | MIT |
| `tslib` | 2.8.1 | 0BSD |
| `tw-animate-css` | 1.4.0 | MIT |
| `use-callback-ref` | 1.3.3 | MIT |
| `use-sidecar` | 1.1.3 | MIT |

## Fonts

The console bundles the Inter and JetBrains Mono typefaces (`@fontsource/inter`,
`@fontsource/jetbrains-mono`), both under the SIL Open Font License 1.1. The OFL
text is in `licenses/texts/common/OFL-1.1.txt` and ships alongside the fonts in
the built console. Reserved Font Names must not be reused by derived works.

## UI packages that publish no licence file

These declare a licence in `package.json` but ship no licence file in their npm
tarball. The declared licence and copyright holder are recorded here, and the
canonical text for that licence is in `licenses/texts/common/`.

| package | version | declared licence | copyright holder |
|---|---|---|---|
| `@radix-ui/number` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/primitive` | 1.1.1 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-alert-dialog` | 1.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-arrow` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-checkbox` | 1.1.4 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-collection` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-compose-refs` | 1.1.1, 1.1.3 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-context` | 1.1.1 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-dialog` | 1.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-direction` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-dismissable-layer` | 1.1.5 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-dropdown-menu` | 2.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-focus-guards` | 1.1.1 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-focus-scope` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-id` | 1.1.0, 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-label` | 2.1.11 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-menu` | 2.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-popover` | 1.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-popper` | 1.2.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-portal` | 1.1.4 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-presence` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-primitive` | 2.0.2, 2.1.6, 2.1.7 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-radio-group` | 1.2.3 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-roving-focus` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-select` | 2.1.6 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-slot` | 1.1.2, 1.3.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-tabs` | 1.1.3 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-callback-ref` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-controllable-state` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-escape-keydown` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-layout-effect` | 1.1.0, 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-previous` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-rect` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-use-size` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/react-visually-hidden` | 1.1.2 | MIT | https://radix-ui.com/primitives |
| `@radix-ui/rect` | 1.1.0 | MIT | https://radix-ui.com/primitives |
| `@types/prop-types` | 15.7.15 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped/tree/master/types/prop-types |
| `@types/react` | 18.3.12 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped/tree/master/types/react |
| `@types/react-dom` | 18.3.1 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped/tree/master/types/react-dom |
| `@xterm/xterm` | 6.0.0 | MIT | https://github.com/xtermjs/xterm.js#readme |
| `prop-types` | 15.8.1 | MIT | https://facebook.github.io/react/ |
| `react` | 18.3.1 | MIT | https://reactjs.org/ |
| `react-dom` | 18.3.1 | MIT | https://reactjs.org/ |
| `react-is` | 16.13.1 | MIT | https://reactjs.org/ |
| `react-remove-scroll-bar` | 2.3.8 | MIT | Anton Korzunov |
| `scheduler` | 0.23.2 | MIT | https://reactjs.org/ |

## Components invoked as separate processes, not linked

The agent container images apt-install `asciinema` (GPL-3.0), which `wardyn-rec`
executes as a subprocess and never links. Publishing those images nonetheless
conveys that binary; see `deploy/images/THIRD-PARTY-GPL.md` for the corresponding
source offer covering it and every other GPL/LGPL package in the base images.
