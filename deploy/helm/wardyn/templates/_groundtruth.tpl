{{/*
Names the Kubernetes ground-truth sensor (groundtruth.k8s.*) shares between
groundtruth.yaml, rbac.yaml, networkpolicy.yaml and deployment.yaml, so wardynd's
WARDYN_GROUNDTRUTH_TOKEN_SECRET, the Secret the DaemonSet mounts and the Role's
resourceNames cannot drift apart. `| default dict` makes an absent key (an old
--reuse-values map) read as off.
*/}}
{{- define "wardyn.groundtruthEnabled" -}}
{{- if ((.Values.groundtruth | default dict).k8s | default dict).enabled -}}true{{- end -}}
{{- end -}}

{{/* The sensor's namespace: groundtruth.k8s.namespace, else the release namespace. */}}
{{- define "wardyn.groundtruthNamespace" -}}
{{- (((.Values.groundtruth | default dict).k8s | default dict).namespace) | default .Release.Namespace -}}
{{- end -}}

{{/*
Every sensor object is named from the release name AND the release namespace.
Two releases that share a name (wardyn in prod and wardyn in staging) and share
one sensor namespace would otherwise render the same Secret, DaemonSet, Role and
RoleBinding there and overwrite each other. A name over 63 characters is cut to
54 and given a hash of the full name, so it stays unique and valid. Pass
(dict "ctx" . "suffix" "groundtruth").
*/}}
{{- define "wardyn.groundtruthName" -}}
{{- $full := printf "%s-%s-%s" (include "wardyn.fullname" .ctx) .ctx.Release.Namespace .suffix -}}
{{- if le (len $full) 63 -}}
{{- $full -}}
{{- else -}}
{{- printf "%s-%s" ($full | trunc 54 | trimSuffix "-") (sha256sum $full | trunc 8) -}}
{{- end -}}
{{- end -}}

{{/* The one Secret wardynd writes the sensor's token and CA into; also the DaemonSet's name. */}}
{{- define "wardyn.groundtruthSecret" -}}
{{- include "wardyn.groundtruthName" (dict "ctx" . "suffix" "groundtruth") -}}
{{- end -}}

{{/* The Role and RoleBinding that let wardynd patch that Secret. */}}
{{- define "wardyn.groundtruthRole" -}}
{{- include "wardyn.groundtruthName" (dict "ctx" . "suffix" "groundtruth-token") -}}
{{- end -}}

{{/*
The sensor pods' own labels. The control-plane namespace is part of them, so one
release's NetworkPolicy peer and DaemonSet selector never match another release's
sensor in a shared sensor namespace.
*/}}
{{- define "wardyn.groundtruthLabels" -}}
app.kubernetes.io/name: {{ printf "%s-groundtruth" (include "wardyn.name" .) }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: groundtruth
wardyn.dev/control-plane-namespace: {{ .Release.Namespace }}
{{- end -}}
