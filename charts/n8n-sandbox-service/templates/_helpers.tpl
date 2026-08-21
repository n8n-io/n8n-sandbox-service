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

{{/*
Runner resource name. The name caps at 61 characters, not 63: the StatefulSet
appends "-<ordinal>" to build each pod name, and the controller copies that
pod name into the pod's hostname, which Kubernetes limits to 63 characters. A
63-character runner name therefore renders a StatefulSet that can never create
a pod. 61 leaves room for ordinals 0-9. Use fullnameOverride to shorten the
name further if you run more than 10 runner replicas.

The privileged name keeps the suffix whole and adds a hash of the full name
when the base must be truncated, so two long release names cannot produce the
same runner.
*/}}
{{- define "n8n-sandbox-service.runnerName" -}}
{{- $fullname := include "n8n-sandbox-service.fullname" . -}}
{{- $maxLength := 61 -}}
{{- if eq .Values.runner.isolation "privileged" -}}
{{- $suffix := "-privileged-runner" -}}
{{- $baseLength := sub $maxLength (len $suffix) | int -}}
{{- if le (len $fullname) $baseLength -}}
{{- printf "%s%s" $fullname $suffix -}}
{{- else -}}
{{- $hash := sha256sum $fullname | trunc 8 -}}
{{- $base := $fullname | trunc (sub $baseLength 9 | int) | trimSuffix "-" -}}
{{- printf "%s-%s%s" $base $hash $suffix -}}
{{- end -}}
{{- else -}}
{{- printf "%s-sysbox-runner" $fullname | trunc $maxLength | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
Mount path of the inner Docker data root volume. With disk quotas the runner
mounts a loopback xfs image at /var/lib/docker, so the volume has to hold that
image one level up instead. Otherwise the image lands on the container
filesystem, where no volume limit bounds it.
*/}}
{{- define "n8n-sandbox-service.runnerDockerDataMountPath" -}}
{{- if gt (int .Values.runner.config.defaultDiskQuotaMb) 0 -}}
/var/lib/docker-pool
{{- else -}}
/var/lib/docker
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
