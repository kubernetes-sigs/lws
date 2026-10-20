{{/*
Expand the name of the chart.
*/}}
{{- define "lws.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "lws.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "lws.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "lws.labels" -}}
helm.sh/chart: {{ include "lws.chart" . }}
{{ include "lws.nameLabel" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "lws.selectorLabels" -}}
{{ include "lws.nameLabel" . }}
{{ include "lws.instanceLabel" . }}
{{- end }}

{{/*
Name label
*/}}
{{- define "lws.nameLabel" -}}
app.kubernetes.io/name: {{ include "lws.name" . }}
{{- end }}

{{/*
Instance label
*/}}
{{- define "lws.instanceLabel" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the controller manager service account
*/}}
{{- define "lws.serviceAccountName" -}}
{{- default (printf "%s-controller-manager" (include "lws.fullname" .)) .Values.serviceAccount.name }}
{{- end }}

{{/*
Controller manager image reference. A digest, when set, takes precedence over the tag.
*/}}
{{- define "lws.image" -}}
{{- $image := .Values.image.manager }}
{{- if $image.digest }}
{{- printf "%v@%s" $image.repository $image.digest }}
{{- else }}
{{- printf "%v:%v" $image.repository (default .Chart.AppVersion $image.tag) }}
{{- end }}
{{- end }}
