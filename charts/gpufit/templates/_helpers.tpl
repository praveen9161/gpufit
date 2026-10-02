{{- define "gpufit.labels" -}}
app.kubernetes.io/name: gpufit
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "gpufit.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "gpufit.validate" -}}
{{- if not (has .Values.mode (list "observe" "enforce")) }}
{{- fail (printf "mode must be observe or enforce, got %q" .Values.mode) }}
{{- end }}
{{- if not (has .Values.agent.collector (list "nvidia-smi" "fake")) }}
{{- fail (printf "agent.collector must be nvidia-smi or fake, got %q" .Values.agent.collector) }}
{{- end }}
{{- end }}
