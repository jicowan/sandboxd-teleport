{{/* Common labels */}}
{{- define "sandboxd.labels" -}}
app.kubernetes.io/part-of: sandboxd
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Fully-qualified image ref for a component repo */}}
{{- define "sandboxd.image" -}}
{{- printf "%s/%s:%s" .registry .repo .tag -}}
{{- end -}}

{{/* Operator image */}}
{{- define "sandboxd.operatorImage" -}}
{{- include "sandboxd.image" (dict "registry" .Values.image.registry "repo" .Values.operator.repo "tag" .Values.image.tag) -}}
{{- end -}}

{{/* Router image */}}
{{- define "sandboxd.routerImage" -}}
{{- include "sandboxd.image" (dict "registry" .Values.image.registry "repo" .Values.router.repo "tag" .Values.image.tag) -}}
{{- end -}}

{{/* Worker image (passed to the operator as --worker-image) */}}
{{- define "sandboxd.workerImage" -}}
{{- include "sandboxd.image" (dict "registry" .Values.image.registry "repo" .Values.worker.repo "tag" .Values.image.tag) -}}
{{- end -}}

{{/*
waitFor renders an initContainer that blocks until a set of host:port deps accept a
TCP connection — so a pod never starts its main container before its dependencies are
up (valkey before operator/router; operator before router). Runtime-robust: it also
re-gates after a pod restart, not just at install time. Arg: a dict with:
  name  = initContainer name
  image = busybox-ish image with `nc`
  deps  = list of "host:port" strings
*/}}
{{- define "sandboxd.waitFor" -}}
- name: {{ .name }}
  image: {{ .image }}
  command:
    - sh
    - -c
    - |
      {{- range .deps }}
      echo "waiting for {{ . }} ..."
      until nc -z {{ regexReplaceAll ":" . " " }}; do sleep 2; done
      echo "{{ . }} is up"
      {{- end }}
{{- end -}}
