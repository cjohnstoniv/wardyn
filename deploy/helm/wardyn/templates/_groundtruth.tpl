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

{{/* The one Secret wardynd writes the sensor's token and CA into. */}}
{{- define "wardyn.groundtruthSecret" -}}
{{- printf "%s-groundtruth" (include "wardyn.fullname" .) -}}
{{- end -}}
