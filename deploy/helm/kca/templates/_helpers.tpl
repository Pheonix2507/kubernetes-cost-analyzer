{{/*
Helpers.

WHY A HELPERS FILE AT ALL
Names appear in a dozen places -- Deployment, Service, ServiceAccount, RBAC, selectors, ServiceMonitor
-- and a selector that disagrees with a pod label by one character produces a Deployment whose pods
nothing selects. That failure is silent: the pods run, the Service has no endpoints, and every request
gets connection refused. Computing names once removes the possibility.
*/}}

{{/* The release-scoped base name, truncated to 63 characters because that is the Kubernetes label limit. */}}
{{- define "kca.name" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Labels applied to every object.

The app.kubernetes.io/* set is the documented convention, and following it is not cosmetic: `kubectl
get all -l app.kubernetes.io/instance=<release>` finds everything this release owns, which is what makes
a release inspectable and deletable as a unit.
*/}}
{{- define "kca.labels" -}}
app.kubernetes.io/name: kubernetes-cost-analyzer
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kubernetes-cost-analyzer
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/*
SELECTOR labels: a strict SUBSET of the above, and the subset matters.

A Deployment's selector is IMMUTABLE after creation. So anything that changes between releases --
version, chart version -- must be excluded, or the next `helm upgrade` fails with "field is immutable"
and the only fix is deleting the Deployment. That is why version labels are in kca.labels and not here.
*/}}
{{- define "kca.selectorLabels" -}}
app.kubernetes.io/name: kubernetes-cost-analyzer
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The image reference for one component. */}}
{{- define "kca.image" -}}
{{- $root := index . 0 -}}
{{- $component := index . 1 -}}
{{- $repo := printf "%s/%s" $root.Values.image.registry $component -}}
{{- printf "%s:%s" $repo (required "image.tag is required -- see the note in values.yaml about never using latest" $root.Values.image.tag) -}}
{{- end -}}

{{/*
The name of the Secret holding the database URL and API keys.

Either an existing Secret the operator created out of band, or one this chart creates from values. See
the warning in templates/secret.yaml about why the second is development-only.
*/}}
{{- define "kca.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "kca.name" .) -}}
{{- end -}}
{{- end -}}

{{- define "kca.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kca.name" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Environment shared by all three binaries.

Defined once because the three read the SAME config package: a variable added for the API that the
collector needs too would otherwise be added in one template and forgotten in the other, and the
symptom is a validation error at pod start rather than anything the chart could catch.
*/}}
{{- define "kca.commonEnv" -}}
- name: APP_ENV
  value: {{ .Values.env | quote }}
- name: CLUSTER_NAME
  {{/*
    Required, with no default.
    A Phase 7 audit found this silently defaulting to the placeholder "default", which wrote 74,925 rows
    attributed to a cluster nobody would recognise. config.Validate now refuses the placeholder in
    production, and `required` here fails at TEMPLATE time instead -- which is strictly better, because a
    chart that cannot render is noticed before anything is deployed.
  */}}
  value: {{ required "clusterName is required: it is denormalised onto every cost row and must be stable for the life of the cluster" .Values.clusterName | quote }}
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "kca.secretName" . }}
      key: database-url
- name: PRICING_CATALOGUE_PATH
  value: /etc/kca/catalogue.yaml
- name: PROMETHEUS_URL
  value: {{ required "prometheus.url is required: the collector reads usage from it" .Values.prometheus.url | quote }}
{{- end -}}

{{/*
Pod-level security context.

EVERY FIELD HERE IS A CVE CLASS, not box-ticking:
  runAsNonRoot         -- a container escape lands as an unprivileged user rather than as node root
  seccompProfile       -- RuntimeDefault blocks ~40 syscalls a Go binary never issues, which is the
                          difference between an exploit needing a kernel bug and needing a kernel bug
                          that survives seccomp
  fsGroup is ABSENT    -- deliberately. These images have no writable volumes, so setting it would make
                          the kubelet recursively chown a volume that does not exist.
This satisfies the `restricted` Pod Security Standard, which means the chart installs into a namespace
with PSA enforcement enabled rather than being rejected.
*/}}
{{- define "kca.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "kca.containerSecurityContext" -}}
allowPrivilegeEscalation: false
{{/*
  readOnlyRootFilesystem with NO writable volume mounted, which only works because the images are
  distroless-static: no shell, no package manager, nothing that writes to /tmp. A container that needed
  scratch space would need an emptyDir here, and the fact that these do not is a property of how they
  were built rather than luck.
*/}}
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}
