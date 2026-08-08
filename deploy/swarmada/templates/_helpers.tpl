# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 The Swarmada Authors <maintainers@swarmada.io>

{{/*
Common template helpers for the Swarmada chart.
These follow the standard Helm library-chart idioms so every template names
resources and labels consistently.
*/}}

{{/* Chart name, overridable via nameOverride (truncated to the 63-char DNS limit). */}}
{{- define "swarmada.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. fullnameOverride wins; otherwise release+chart name,
collapsing the common case where the release is already named "swarmada".
*/}}
{{- define "swarmada.fullname" -}}
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

{{/* chart label "name-version" (version ':' is illegal in labels → '_'). */}}
{{- define "swarmada.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels — the immutable subset used in Deployment/Service selectors. */}}
{{- define "swarmada.selectorLabels" -}}
app.kubernetes.io/name: {{ include "swarmada.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Full label set for metadata (recommended Kubernetes labels + control-plane). */}}
{{- define "swarmada.labels" -}}
helm.sh/chart: {{ include "swarmada.chart" . }}
{{ include "swarmada.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: swarmada
control-plane: controller-manager
{{- end -}}

{{/* ServiceAccount name: honor an externally-managed SA when create=false. */}}
{{- define "swarmada.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-manager" (include "swarmada.fullname" .)) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Resolved controller-manager image reference. tag falls back to the chart
appVersion so a plain `helm install` pins to a real version, not :latest.
*/}}
{{- define "swarmada.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* Name of the webhook serving-cert Secret (cert-manager fills it or you supply it). */}}
{{- define "swarmada.webhookCertSecret" -}}
{{- printf "%s-webhook-server-cert" (include "swarmada.fullname" .) -}}
{{- end -}}
