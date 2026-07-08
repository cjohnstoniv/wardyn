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
