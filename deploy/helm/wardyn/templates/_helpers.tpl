{{/*
Template helpers for the Wardyn chart.

Finding: the chart shipped with Chart.yaml/values.yaml/README.md but NO
templates/, so `helm install wardyn ./deploy/helm/wardyn` rendered zero objects
despite the README calling it "the ONE blessed Kubernetes deployment path".
These helpers give every rendered object a stable name and a consistent label
set so the Deployment selector, Service selector, ServiceAccount reference,
Secret reference, and NetworkPolicy podSelector all agree.
*/}}

{{/* Base name, truncated to the 63-char DNS-1123 limit. */}}
{{- define "wardyn.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. If the release name already contains the chart name
we avoid doubling it (the standard Helm convention), then truncate to 63 chars.
*/}}
{{- define "wardyn.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Chart name + version, used for the helm.sh/chart label. */}}
{{- define "wardyn.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels: the minimal, immutable set the Deployment selector uses. */}}
{{- define "wardyn.selectorLabels" -}}
app.kubernetes.io/name: {{ include "wardyn.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Full label set applied to every object's metadata. */}}
{{- define "wardyn.labels" -}}
helm.sh/chart: {{ include "wardyn.chart" . }}
{{ include "wardyn.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: control-plane
app.kubernetes.io/part-of: wardyn
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* ServiceAccount name to use (created one, or an externally-supplied one). */}}
{{- define "wardyn.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "wardyn.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret that holds the Postgres DSN (and optionally the age key).
When postgres.dsn.secretRef.name is set the operator manages that Secret
externally; otherwise the chart creates one named "<fullname>-secrets" from the
inline (demo-grade) values.
*/}}
{{- define "wardyn.secretName" -}}
{{- if .Values.postgres.dsn.secretRef.name -}}
{{- .Values.postgres.dsn.secretRef.name -}}
{{- else -}}
{{- printf "%s-secrets" (include "wardyn.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret that holds the admin bearer token. Deliberately SEPARATE
from wardyn.secretName: the DSN defaults to an external Secret, so sharing the
name would emit a secretKeyRef for an "admin-token" key the operator's Postgres
Secret does not carry — CreateContainerConfigError. The two credentials pick
their mode independently.
*/}}
{{- define "wardyn.authSecretName" -}}
{{- if .Values.auth.adminToken.secretRef.name -}}
{{- .Values.auth.adminToken.secretRef.name -}}
{{- else -}}
{{- printf "%s-auth" (include "wardyn.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Non-empty when SOME authentication is wired. Without one, internal/api/http.go
adminAuth 401s every API route ("admin token not configured; public API
disabled") while /healthz keeps reporting Ready — a pod that looks healthy and
serves nothing. templates/secret.yaml turns that into a render-time failure.
Every escape hatch counts: the chart's two adminToken modes, and an
operator-supplied token/issuer in .Values.env or .Values.extraEnv.
*/}}
{{- define "wardyn.authConfigured" -}}
{{- $ok := or .Values.auth.adminToken.secretRef.name .Values.auth.adminToken.value (hasKey .Values.env "WARDYN_ADMIN_TOKEN") (hasKey .Values.env "WARDYN_OIDC_ISSUER") -}}
{{- range .Values.extraEnv -}}
{{- if has (.name | default "") (list "WARDYN_ADMIN_TOKEN" "WARDYN_OIDC_ISSUER") -}}{{- $ok = true -}}{{- end -}}
{{- end -}}
{{- if $ok }}true{{ end -}}
{{- end -}}

{{/*
Refuse at render when an optional port collides with the console's HTTP port.
wardynd refuses to BOOT when uiSandbox.port equals service.port (the separate
browser origin is the control — cmd/wardynd/main.go validateUISandboxConfig),
and a Service carrying the same port number twice is rejected by the API server
anyway. Both are failures that only show up after apply — as a crash-loop or a
rejected object — so say it at render, like every other guard in this chart.
*/}}
{{- define "wardyn.assertPorts" -}}
{{- $http := int .Values.service.port -}}
{{- $ssh := .Values.ssh | default dict -}}
{{- $ui := .Values.uiSandbox | default dict -}}
{{- if and $ssh.enabled (eq (int ($ssh.port | default 2222)) $http) -}}
{{- fail (printf "wardyn: ssh.port and service.port are both %d — the SSH gateway and the console cannot share one port. Give ssh.port its own number." $http) -}}
{{- end -}}
{{- if and $ui.enabled (eq (int ($ui.port | default 8081)) $http) -}}
{{- fail (printf "wardyn: uiSandbox.port and service.port are both %d — wardynd refuses to boot when they are equal, because the sandbox's own pages must land on a DIFFERENT browser origin than the console. Give uiSandbox.port its own number." $http) -}}
{{- end -}}
{{- end -}}
