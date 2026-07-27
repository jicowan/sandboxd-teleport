{{/* Common labels */}}
{{- define "frontdoor.labels" -}}
app.kubernetes.io/part-of: sandboxd
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/*
Resolved OIDC issuer. When keycloak.deploy=true and oidc.issuer is left at the
placeholder/empty, callers should set oidc.issuer explicitly to the reachable
Keycloak issuer; we do not guess an in-cluster issuer URL (tokens embed the
issuer the client saw). So this simply returns oidc.issuer.
*/}}
{{- define "frontdoor.issuer" -}}
{{- required "oidc.issuer is required" .Values.oidc.issuer -}}
{{- end -}}

{{/* Resolved JWKS URL: oidc.jwksUrl if set, else <issuer>/protocol/openid-connect/certs */}}
{{- define "frontdoor.jwksUrl" -}}
{{- if .Values.oidc.jwksUrl -}}
{{- .Values.oidc.jwksUrl -}}
{{- else -}}
{{- printf "%s/protocol/openid-connect/certs" (include "frontdoor.issuer" .) -}}
{{- end -}}
{{- end -}}

{{/* Broker Service DNS (in the release namespace) */}}
{{- define "frontdoor.brokerHost" -}}
{{- printf "sandboxd-broker.%s.svc.cluster.local" .Release.Namespace -}}
{{- end -}}

{{/* Router URL in the control-plane namespace */}}
{{- define "frontdoor.routerUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" .Values.controlPlane.routerService .Values.controlPlane.namespace (.Values.controlPlane.routerPort | int) -}}
{{- end -}}

{{/* Broker image */}}
{{- define "frontdoor.brokerImage" -}}
{{- $tag := .Values.broker.tag | default "latest" -}}
{{- $reg := required "image.registry is required for the broker image" .Values.image.registry -}}
{{- printf "%s/%s:%s" $reg .Values.broker.repo $tag -}}
{{- end -}}

{{/* SANDBOXD_APPS JSON registry, built from .Values.apps */}}
{{- define "frontdoor.appsJson" -}}
{{- $m := dict -}}
{{- range .Values.apps -}}
{{- $_ := set $m .id (dict "appTemplate" .appTemplate "pool" .pool "group" .group) -}}
{{- end -}}
{{- $m | toJson -}}
{{- end -}}

{{/*
waitForTCP renders an initContainer that blocks until each host:port accepts a
TCP connection. Runtime-robust: re-gates after a pod restart, not just install.
Arg dict: name, image, deps (list of "host:port").
*/}}
{{- define "frontdoor.waitForTCP" -}}
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

{{/*
waitForHttp renders an initContainer that blocks until an HTTP(S) URL returns
2xx/3xx — used to gate on Keycloak's OIDC discovery (so JWKS is actually being
served, not just the port open). Arg dict: name, image, url.
*/}}
{{- define "frontdoor.waitForHttp" -}}
- name: {{ .name }}
  image: {{ .image }}
  command:
    - sh
    - -c
    - |
      echo "waiting for {{ .url }} ..."
      until wget -q -T 5 -O /dev/null "{{ .url }}"; do sleep 3; done
      echo "{{ .url }} is up"
{{- end -}}
