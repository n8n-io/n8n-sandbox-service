{{/*
Expand the name of the chart.
*/}}
{{- define "n8n-sandbox-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "n8n-sandbox-service.fullname" -}}
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
Chart name and version label.
*/}}
{{- define "n8n-sandbox-service.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "n8n-sandbox-service.apiName" -}}
{{- printf "%s-api" (include "n8n-sandbox-service.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Component label and resource name for the in-cluster runner. Both follow
runner.isolation on purpose: StatefulSet selectors are immutable, so a shared
name would break existing sysbox releases on upgrade. Switching isolation
recreates the runner resources under the other name.
*/}}
{{- define "n8n-sandbox-service.runnerComponent" -}}
{{- if eq .Values.runner.isolation "privileged" -}}privileged-runner{{- else -}}sysbox-runner{{- end -}}
{{- end }}

{{- define "n8n-sandbox-service.runnerName" -}}
{{- $fullname := include "n8n-sandbox-service.fullname" . -}}
{{- $sysboxName := printf "%s-sysbox-runner" $fullname | trunc 63 | trimSuffix "-" -}}
{{- if eq .Values.runner.isolation "privileged" -}}
{{- $suffix := "-privileged-runner" -}}
{{- $baseLength := sub 63 (len $suffix) | int -}}
{{- $privilegedName := printf "%s%s" ($fullname | trunc $baseLength | trimSuffix "-") $suffix -}}
{{- if eq $privilegedName $sysboxName -}}
{{- $suffix = "-privileged-runner-p" -}}
{{- $baseLength = sub 63 (len $suffix) | int -}}
{{- printf "%s%s" ($fullname | trunc $baseLength | trimSuffix "-") $suffix -}}
{{- else -}}
{{- $privilegedName -}}
{{- end -}}
{{- else -}}
{{- $sysboxName -}}
{{- end -}}
{{- end }}

{{- define "n8n-sandbox-service.authSecretName" -}}
{{- default (printf "%s-auth" (include "n8n-sandbox-service.fullname" .)) .Values.auth.existingSecret | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "n8n-sandbox-service.labels" -}}
helm.sh/chart: {{ include "n8n-sandbox-service.chart" . }}
app.kubernetes.io/name: {{ include "n8n-sandbox-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "n8n-sandbox-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "n8n-sandbox-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "n8n-sandbox-service.sandboxImage" -}}
{{- printf "%s:%s" .Values.runner.sandboxImage.repository (.Values.runner.sandboxImage.tag | default .Chart.AppVersion) }}
{{- end }}
