{{- define "runnerd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "runnerd.fullname" -}}
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

{{- define "runnerd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runnerd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: runnerd
{{- end -}}

{{- define "runnerd.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "runnerd.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: wardyn
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Cluster-scoped objects have one global name, so it carries the release namespace. */}}
{{- define "runnerd.clusterName" -}}
{{- printf "%s-%s" .Release.Namespace (include "runnerd.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* runnerd and its proxy are pinned to one version. */}}
{{- define "runnerd.tag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{- define "runnerd.validate" -}}
{{- if not .Values.org -}}
{{- fail "wardyn-runnerd: org is required (the organisation's public HTTPS URL)." -}}
{{- end -}}
{{- if not .Values.runsNamespace -}}
{{- fail "wardyn-runnerd: runsNamespace is required: a pre-existing namespace dedicated to this runner's runs." -}}
{{- end -}}
{{- if eq .Values.runsNamespace .Release.Namespace -}}
{{- fail "wardyn-runnerd: runsNamespace must not be the release namespace: a run pod there could name runnerd's ServiceAccount." -}}
{{- end -}}
{{- if not (has .Values.sandbox.arch (list "amd64" "arm64")) -}}
{{- fail "wardyn-runnerd: sandbox.arch must be amd64 or arm64; a mixed cluster installs one release per arch." -}}
{{- end -}}
{{- if not .Values.registration.existingSecret -}}
{{- fail "wardyn-runnerd: registration.existingSecret is required: a Secret holding the registration token under the key \"token\"." -}}
{{- end -}}
{{- range $key := list "allowUnenforcedNetPol" "ackAmbientDefaultDeny" -}}
{{- if hasKey ($.Values.k8s | default dict) $key -}}
{{- fail (printf "wardyn-runnerd: k8s.%s is refused: runnerd boots only on an enforced NetworkPolicy canary verdict. Use a runs namespace no platform default-deny covers." $key) -}}
{{- end -}}
{{- end -}}
{{- end -}}
