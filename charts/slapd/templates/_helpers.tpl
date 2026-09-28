{{/*
Expand the name of the chart.
*/}}
{{- define "slapd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "slapd.fullname" -}}
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
Common labels.
*/}}
{{- define "slapd.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "slapd.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "slapd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "slapd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Child spec passthrough: pin clusterRef, hand the rest through untouched.

Copied from charts/slapd-mesh rather than shared through a library chart, and
that is deliberate. The two charts label their objects on OPPOSITE principles —
slapd-mesh.labels is free of .Release.Name because mesh objects must be
byte-identical at every site, while slapd.labels uses it because a single-site
release name is exactly the right instance identity. A shared helper would have
to pick one and be wrong for the other chart. Three lines of duplication is the
cheaper mistake.
*/}}
{{- define "slapd.childSpec" -}}
{{- $spec := omit (default (dict) .spec) "clusterRef" -}}
{{- toYaml (merge (dict "clusterRef" .clusterName) $spec) -}}
{{- end -}}

{{/*
Survive `helm uninstall`.

A SlapdDatabase is not a deployment artifact — it is the declaration of a
directory that holds data. Helm's default lifecycle would delete these CRs with
the release, and what that costs depends on a field the chart does not own: with
cleanupPolicy Retain (the default, ADR-005) the database keeps serving on every
pod, merely unmanaged; with Delete the operator tears it, its overlays and its
accesslog DB off every pod.

Before this chart rendered databases at all, `helm uninstall slapd` removed the
SlapdCluster and left SlapdDatabase CRs standing, because they were separate
objects nobody had handed to Helm. Adding them to the chart would have QUIETLY
changed that, which is the kind of regression a convenience feature has no
business introducing. The annotation keeps the old behaviour.

The PVCs were never Helm's either way: volumeClaimTemplates are not garbage
collected, which is what makes a reinstall over surviving storage possible at
all (and is its own trap — see docs/BACKLOG.md).
*/}}
{{- define "slapd.retainAnnotations" -}}
{{- if .Values.retainOnUninstall -}}
helm.sh/resource-policy: keep
{{- end -}}
{{- end -}}

{{/*
Render-time validation for the databases this chart declares.

One rule, and it is the one that survives from the mesh chart's four: two
databases must not share a ridBase. ADR-003 requires one per database, because
RIDs are derived from it (in-cluster peer i → ridBase+i+1, external peer j →
ridBase+50+j+1) and a collision makes slapd confuse two databases' per-consumer
cookie state. The mesh chart's other three checks are about sites and do not
exist here.

Note what this is NOT: docs/BACKLOG.md tracks ridBase uniqueness as documented
but unenforced, and the real fix is a reconcile-time check in the SlapdDatabase
controller, which also catches CRs applied by hand. This catches only what the
chart itself renders — worth having because it is free, not a substitute.
*/}}
{{- define "slapd.validateDatabases" -}}
{{- $ridBases := list -}}
{{- range $i, $db := .Values.databases -}}
  {{- if not $db.name -}}
  {{- fail (printf "slapd: databases[%d] has no name" $i) -}}
  {{- end -}}
  {{- $repl := default (dict) (default (dict) $db.spec).replication -}}
  {{- if and (hasKey $repl "ridBase") (not (kindIs "invalid" $repl.ridBase)) -}}
    {{- $rid := printf "%v" $repl.ridBase -}}
    {{- if has $rid $ridBases -}}
    {{- fail (printf "slapd: ridBase %s is used by two databases (at databases[%d], %s). ADR-003 requires a unique ridBase per database within a cluster." $rid $i $db.name) -}}
    {{- end -}}
    {{- $ridBases = append $ridBases $rid -}}
  {{- end -}}
{{- end -}}
{{- end -}}
