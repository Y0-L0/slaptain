/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package obsmetrics renders one reconcile pass's observations as metric
// samples (ADR-033 decision 3 — Surface 2, the quantities only the operator
// can observe because only it performed the cross-pod comparison). No
// Kubernetes types, no Prometheus types, no I/O; stdlib only.
//
// The renders are pure functions. The ONE piece of state in the package is
// Store (store.go), which holds the most recent observation per object between
// the reconcile pass that took it and the scrape that reads it — the two clocks
// Surface 2 has to bridge, since nothing here is fresher than the operator's
// 60 s tick and a scrape must not go and look for itself. It costs the package
// a sync import and nothing else: still no Kubernetes types, still no I/O.
//
// It lives here rather than in internal/controller because it belongs to the
// metrics concern end to end — the observation types it holds are defined here,
// the absence rules it must not violate are enforced here, and a reviewer
// asking "what does Surface 2 export and from when" finds the whole answer in
// one package. internal/controller is ~110 files of reconcile logic, and a
// store buried there would be read as reconcile state, which it is not: no
// controller decision may depend on it.
//
// The rule this package exists to enforce is ADR-033 decision 5: a reading the
// operator could not take is ABSENT from the render — never a 0, and never the
// last known value. A gauge reporting 0 for "no evidence" produces an alert
// that falls silent exactly when the thing it watches has broken, and that is
// the failure mode this surface is most able to introduce and least able to
// detect. So absence is expressed by emitting no sample, and every series
// family here states its own absence rule in its doc comment.
//
// The hardest half of decision 5 is that 0 is sometimes the right answer, and
// the two cases are deliberately rendered differently:
//
//	SuffixProbeOK      — 0 means "probed, and the answer was bad" (ADR-025).
//	                     Not probed emits nothing.
//	CSNSpreadSeconds   — 0 means "two or more pods answered and they agree".
//	                     No measurement at all emits nothing, because 0 would
//	                     read as perfect convergence. That case is a nil
//	                     DatabaseCSN.Spread and nothing else: the pointer is
//	                     the gate, so the two readings cannot be confused by a
//	                     caller, by this render, or by anything between them.
//	CSNPairsRead       — 0 means "the database was attempted and every read
//	                     failed", which is the 2026-10-06 "6 of 6" incident and
//	                     the one case where the ATTEMPT is the observation.
//
// Two quantities deliberately do NOT live here, and both are decision 1 (a
// metric belongs to the vantage point that observes it DIRECTLY):
//
//   - Withheld syncrepl convergence. It is the SlapdDatabase condition
//     SyncreplStanzasReady, so Surface 1 already exports it as
//     slaptain_database_condition{condition="SyncreplStanzasReady",reason=…},
//     and does it BETTER: kube-state-metrics carries the Unknown case, which a
//     presence-is-the-signal gauge structurally cannot. A slaptain_syncrepl_withheld
//     family existed here until 2026-10-06 and was removed on exactly that
//     ground. If you are about to re-add it: the question is not whether the
//     operator knows, it is whether the operator is the only one who knows.
//   - Any other CR status condition. Same rule, same answer.
//
// SuffixGlue is the near miss that is NOT a violation, and the boundary is
// worth stating because it will be questioned. Surface 1 exports the database's
// DataPresent condition, whose GlueSuffix reason is the AGGREGATE verdict over
// pods. WHICH pod carries the glue appears nowhere in CR status — the operator
// holds it and nothing else does — so the per-pod series is ours and the
// per-database verdict stays Surface 1's. Were DataPresent ever to grow a
// per-pod encoding, this family would have to go.
//
// One metric name carries exactly ONE label shape. The two renders therefore
// have two freshness anchors — slaptain_cluster_observation_timestamp_seconds
// and slaptain_database_observation_timestamp_seconds — rather than one name
// emitted with and without a `database` label. Prometheus permits the varying
// shape; a rule written over it cannot be read, because
// `time() - <anchor> > T` would then fire once per database AND once for the
// cluster, double-alerting a single stalled site. The names also follow Surface
// 1's own slaptain_cluster_* / slaptain_database_* split
// (monitoring/kube-state-metrics-values.yaml), so the scope of a series is
// legible from its name alone. Families that exist at one scope only —
// csn_spread, csn_pairs_*, peer_providers_discovered (cluster) and
// suffix_probe_ok, suffix_glue (database) — keep their unprefixed names: they
// are unambiguous, and renaming them would be churn.
//
// `site` is deliberately NOT a label. The operator knows its own site
// (ADR-028's single per-site fact), but monitoring is site-local (ADR-033
// amendment 2026-10-09): each site's Prometheus scrapes only that site, so the
// Prometheus IS the site and a `site` label would be a constant on every
// series it holds. Worse, it would exist on Surface 2 alone — Surfaces 1 and 3
// carry none — so a join written `on (site, ...)` would silently match
// nothing. Peer sites are visible where the operator reports on them, under
// its own `peer` label. Do not re-add it here.
//
// `mesh` is always present and is the empty string for a cluster with no
// meshRef, which is a legitimate standalone deployment. Note that Surface 1
// differs here: kube-state-metrics OMITS a label it cannot resolve, so the two
// surfaces disagree on the mesh dimension for non-mesh clusters only — where
// there is no cross-site series to join against anyway. An empty label value
// and an absent label are the same thing to Prometheus, so this costs nothing
// at query time.
//
// A healthy three-pod, two-database, two-peer site renders, in the order the
// contract promises:
//
//	slaptain_cluster_observation_timestamp_seconds{cluster="slapd",mesh="ldap",namespace="slaptain-testing"} 1.7e+09
//	slaptain_csn_pairs_expected{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 3
//	slaptain_csn_pairs_expected{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 3
//	slaptain_csn_pairs_read{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 3
//	slaptain_csn_pairs_read{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 3
//	slaptain_csn_spread_seconds{cluster="slapd",database="db2",mesh="ldap",namespace="slaptain-testing"} 0.5
//	slaptain_csn_spread_seconds{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 0
//	slaptain_peer_providers_discovered{cluster="slapd",mesh="ldap",namespace="slaptain-testing",peer="site-2"} 3
//	slaptain_peer_providers_discovered{cluster="slapd",mesh="ldap",namespace="slaptain-testing",peer="site-3"} 2
//
// and its `default` database, whose three pods agree, carries a spread of 0 —
// a measurement, not a silence. The same site's database pass renders:
//
//	slaptain_database_observation_timestamp_seconds{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing"} 1.7e+09
//	slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-0",role="readwrite"} 1
//	slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-1",role="readwrite"} 1
//	slaptain_suffix_probe_ok{cluster="slapd",database="default",mesh="ldap",namespace="slaptain-testing",pod="slapd-ro-0",role="readonly"} 1
//
// This is the single implementation of the render. Nothing above a Sample is
// decided here: binding these samples to a collector, a registry or a
// /metrics handler is a separate, impure adapter, which is what keeps this
// package exactly as testable as internal/ridcoord. The Store the adapter
// reads from is here; the adapter itself, and with it client_golang, is not.
package obsmetrics

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Metric names. Exported because three things have to agree on them — the
// renders below, the Prometheus adapter that will consume them, and
// monitoring/prometheus-rules.yaml — and a string literal copied three times
// is how a rule comes to key on a series that no longer exists.
const (
	// MetricClusterObservationTimestamp is the cluster pass's freshness anchor:
	// the Unix time of the pass that produced the rest of RenderCluster's
	// samples. Everything on this surface inherits the operator's 60 s tick
	// (ADR-033 Consequences), so staleness is detectable only through this
	// series — every other one would hold its last value and read healthy.
	MetricClusterObservationTimestamp = "slaptain_cluster_observation_timestamp_seconds"

	// MetricDatabaseObservationTimestamp is the database pass's freshness
	// anchor, and a distinct name rather than the cluster anchor with a
	// `database` label: one metric name, one label shape (see the package
	// comment), so `time() - <anchor> > T` is writable at each scope without
	// firing twice for one stalled site.
	//
	// T is NOT the same for the two anchors, and neither is 300. The shipped
	// rules and the arithmetic behind each threshold are in
	// monitoring/prometheus-rules.yaml; the short version is that a healthy
	// SlapdDatabase requeues on a five-minute resync floor, so this anchor's
	// threshold has to clear that floor PLUS the pass's own per-pod LDAP sweep,
	// while the cluster anchor needs a gate rather than a bigger number,
	// because a non-replicated cluster has no periodic requeue at all
	// (ADR-002 amendment) and no threshold saves it.
	MetricDatabaseObservationTimestamp = "slaptain_database_observation_timestamp_seconds"

	// MetricCSNSpread is the measured per-database contextCSN spread across the
	// read-write pods that answered: newest minus oldest, in seconds. The
	// quantity ReplicationConverged reduces to a boolean (ADR-008 amendment).
	MetricCSNSpread = "slaptain_csn_spread_seconds"

	// MetricCSNPairsRead counts the (pod, database) pairs that yielded a
	// readable contextCSN for one database this pass.
	MetricCSNPairsRead = "slaptain_csn_pairs_read"

	// MetricCSNPairsExpected counts the (pod, database) pairs the pass set out
	// to read for one database. `read < expected` is the alertable condition,
	// which is why these are two absolute counts and not a ratio: a ratio has
	// to divide by a count that can be 0.
	MetricCSNPairsExpected = "slaptain_csn_pairs_expected"

	// MetricPeerProvidersDiscovered counts the remote pods peer discovery found
	// for one external peer. A link that resolved to nothing is how a
	// cross-site outage begins (ADR-031, ADR-032), and `== 0` is that condition
	// exactly.
	//
	// It is ABSENT when discovery could not be asked (DiscoveryRan false), and
	// that is the half most likely to be reinstated as a 0 by a future reader,
	// so here is the whole argument:
	//
	// `discoverRemotePeerProviders` fails for mundane causes — a rotated
	// kubeconfig Secret, an unparseable one, an unset replication network mode,
	// a remote API that will not answer. Rendering 0 there sends a reader to
	// look at a REMOTE site that is fine, for a fault that is entirely local.
	// Zero is reserved for discovery that ran and found nothing.
	//
	// The obvious objection is that `== 0` IS this family's alert, so dropping
	// the sample trades a misattributed alert for silence. It does not, and the
	// reason is a CROSS-SURFACE one: a failed discovery leaves no addresses, so
	// checkPeerCSNConvergence reports the peer Unreachable, and Surface 1
	// publishes slaptain_cluster_peer_connected{...,replication_state=
	// "Unreachable"} 0. The failure is already loud on the surface that owns
	// peer reachability. Surface 2 inventing a provider count to re-say it
	// would be decision 1's overlap as well as decision 5's zero.
	//
	// If that Surface 1 series ever stops existing, this absence stops being
	// safe — which is exactly why the dependency is written down here rather
	// than assumed.
	//
	// There is deliberately no companion "expected" series. `replicasPerPeer`
	// is fan-out — how many connections each local pod opens — not a prediction
	// of how many remote pods discovery ought to find, and under ADR-032 the
	// link count becomes a property of a site PAIR rather than of a pod. Any
	// "expected" the operator could compute here would be derived from
	// discovery itself, so comparing the two would be circular. A non-zero
	// floor is local policy and belongs in an alert rule, where the number is
	// declared by whoever runs the mesh rather than fabricated as an
	// observation.
	MetricPeerProvidersDiscovered = "slaptain_peer_providers_discovered"

	// MetricSuffixProbeOK reports one pod's suffix-entry probe for one database
	// (ADR-025): 1 healthy, 0 for a glue or missing suffix entry. A pod that
	// was not probed is absent — suffixprobe's "positive evidence only"
	// contract carried onto a gauge.
	MetricSuffixProbeOK = "slaptain_suffix_probe_ok"

	// MetricSuffixGlue reports that ONE pod's suffix entry is a glue entry
	// (ADR-025): hidden from ordinary searches, frozen at the winner's
	// entryCSN so every CSN health reading comes back clean, and every backup
	// taken from that pod unrestorable. Value is always 1 and PRESENCE is the
	// signal — a healthy pod emits nothing, because `glue="false"` on every
	// pod of every database is noise on the one family that should never be
	// noisy.
	//
	// Its own family rather than a label on MetricSuffixProbeOK, because glue
	// is a different KIND of fact, not a severity of the same one: a missing
	// suffix entry resolves itself on the next sync, a glue never heals and
	// has a manual runbook. An alert on this family needs no `for:` at all,
	// which is only expressible while it is its own series.
	//
	// It carries `role` all the same, and for a different reason than the probe
	// gauge does. There the label earns a longer fuse for the read-only case;
	// here it earns nothing of the sort — a glue is never the transient state,
	// whichever fleet the pod belongs to. It is on this family to IDENTIFY THE
	// SUBJECT: both families are pod-scoped and the pod is the same pod, so
	// labelling its role on one and not the other reads as an oversight however
	// well a comment explains it, and makes "glue on a read-write pod" a join
	// rather than a selector. ADR-025's sharpest consequence — every backup
	// from the affected pod is unrestorable — is exactly what an operator wants
	// to slice by role.
	MetricSuffixGlue = "slaptain_suffix_glue"
)

// Families is every metric name this package renders — the closed set, in one
// place, so that the adapter's help strings, the decision-1 boundary guard and
// monitoring/prometheus-rules.yaml can all be checked against it instead of
// against three hand-copied lists.
//
// Adding a name here is the moment to answer decision 1's question: does any
// other vantage point already observe this? See the package comment's two
// standing answers.
var Families = []string{
	MetricClusterObservationTimestamp,
	MetricDatabaseObservationTimestamp,
	MetricCSNSpread,
	MetricCSNPairsRead,
	MetricCSNPairsExpected,
	MetricPeerProvidersDiscovered,
	MetricSuffixProbeOK,
	MetricSuffixGlue,
}

// Label names. Same reason as the metric names.
const (
	LabelNamespace = "namespace"
	LabelCluster   = "cluster"
	LabelMesh      = "mesh"
	LabelDatabase  = "database"
	LabelPeer      = "peer"
	LabelPod       = "pod"
	// LabelRole separates a read-write pod from a read-only consumer on the
	// suffix probe. It is a label rather than two families because the fact is
	// the same fact — ADR-025's amendment made DataMissingOnReadOnlyPods its
	// own REASON for exactly this: an RO pod's initial sync is legitimately
	// transient, so a rule gives that case a longer fuse while keeping the
	// read-write case immediate. Cardinality is unchanged: pods × databases
	// was already the bound.
	LabelRole = "role"
)

// Role label values. Two constants rather than two string literals, for the
// reason the metric names are constants: a rule that keys on `role="readOnly"`
// matches nothing and says so nowhere.
const (
	RoleReadWrite = "readwrite"
	RoleReadOnly  = "readonly"
)

// Sample is one metric sample: a name, a label set and a float. Deliberately
// not a Prometheus type — binding to prometheus/client_golang happens in the
// adapter, so this package stays stdlib-only and the render stays a pure
// function of its input.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// String renders the sample in the exposition-like form
// `name{k="v",k2="v2"} value`, with labels sorted by name.
//
// This is a diagnostic and test-comparison form, not the wire format: Sample
// carries a map, so it is not comparable with ==, and a stable string is what
// lets a test name an expected sample set exactly. The adapter reads the
// fields, never this.
func (s Sample) String() string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(s.Name)
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%s=%q", k, s.Labels[k])
	}
	fmt.Fprintf(&b, "} %g", s.Value)
	return b.String()
}

// Identity is the join key every sample carries: the namespace and name of the
// SlapdCluster, plus the mesh it belongs to. Mesh is the empty string for a
// cluster with no meshRef — see the package comment, and note that `site` is
// absent on purpose.
type Identity struct {
	Namespace string
	Cluster   string
	Mesh      string
}

// DatabaseCSN is one database's contextCSN readings from one cluster pass.
//
// Presence of a DatabaseCSN in ClusterObservation.Databases means the database
// WAS attempted, which is why PairsRead and PairsExpected are rendered
// unconditionally — including PairsRead = 0, where the attempt is the
// observation. A database the pass never reached is simply not in the slice.
type DatabaseCSN struct {
	// Database is the SlapdDatabase name, not the suffix: it is the label a
	// reader can take back to `kubectl get sd`.
	Database string
	// PairsExpected is how many (pod, database) pairs the pass set out to read
	// for this database — one per read-write pod.
	PairsExpected int
	// PairsRead is how many of them yielded a readable contextCSN. Zero is a
	// legitimate, renderable value.
	PairsRead int
	// Spread is newest minus oldest contextCSN timestamp over the pods that
	// answered, and is NIL when there was no measurement to take.
	//
	// A pointer, since 2026-10-06, because the plain Duration made the one
	// mistake decision 5 is least able to detect expressible: a database with
	// no measurement at all passed 0, and 0 over two or more pairs renders as
	// perfect convergence — the healthiest reading on the surface, published
	// for a reading nobody took. The render's PairsRead >= 2 gate did not
	// catch it, because PairsRead and Spread came from different places and
	// only agreed by convention.
	//
	// Now they cannot disagree: nil is the whole absence rule, and the render
	// gates on it ALONE. The producer's own invariant — a spread exists
	// exactly when two or more pods were read — is asserted where it is
	// computed (controller.databaseCSNReadouts), which is the only place that
	// can know it.
	Spread *time.Duration
}

// PeerObservation is one external peer as this pass saw it.
type PeerObservation struct {
	// Peer is the externalPeers[].name, or the derived peer name under meshRef.
	Peer string
	// UsesDiscovery is true when the peer resolves its providers through the
	// remote Kubernetes API. The provider count below exists only for those
	// peers: a `uri` or `podAddresses` peer has no discovery result, and
	// rendering 0 found for it would be an outage-shaped reading of a perfectly
	// static configuration.
	UsesDiscovery bool
	// DiscoveryRan is false when discovery could not be ASKED at all: a rotated
	// or unparseable kubeconfig Secret, no replication network mode, the remote
	// API unreachable. ProvidersDiscovered is then meaningless and nothing is
	// rendered for the peer.
	//
	// It is named for what is known, and its zero value is the safe one — the
	// same shape as SuffixProbe.Probed, and for the same reason: on a surface
	// whose rule is "unreadable evidence is absent", a field left unset must
	// mean "no evidence", never "all clear".
	DiscoveryRan bool
	// ProvidersDiscovered is how many remote pods discovery found, and is
	// meaningful only under DiscoveryRan. Zero is then a legitimate, renderable
	// value — discovery ran and resolved to nothing, which is how a cross-site
	// outage begins.
	ProvidersDiscovered int
}

// SuffixProbe is one pod's ADR-025 suffix-entry probe for one database.
//
// Four facts, not one graded number, because every collapse of them loses a
// distinction ADR-025 exists to draw:
//
//	Probed  × Healthy  — "not probed" and "probed, found nothing" must not
//	                     share a value. That is decision 5.
//	Glue               — a glue entry is not a worse `Healthy: false`. It is
//	                     hidden from ordinary search, reads clean on every CSN
//	                     check, makes every backup from that pod unrestorable,
//	                     never heals, and has a manual runbook. It renders as
//	                     its own family.
//	ReadOnly           — an RO consumer mid-initial-sync is LEGITIMATELY
//	                     missing its suffix entry; an RW pod in the same state
//	                     is an incident. ADR-025's 2026-09-14 amendment gave
//	                     that its own condition reason for this reason, and the
//	                     `role` label is the same distinction on this surface.
//	                     Without it an alert on `== 0` either false-fires on
//	                     every RO scale-up or is relaxed until it stops
//	                     catching glue.
type SuffixProbe struct {
	// Pod is the pod name the probe was aimed at.
	Pod string
	// ReadOnly marks a pod of the read-only consumer StatefulSet. Rendered as
	// the `role` label on every probe sample, never as a reason to drop one.
	ReadOnly bool
	// Probed is false when the pod could not be probed at all — transport,
	// bind or ACL failure. Nothing is rendered for it, in either family.
	Probed bool
	// Healthy is the probe verdict and is meaningful only when Probed: true for
	// a real suffix entry, false for a glue or missing one.
	Healthy bool
	// Glue is true when a ManageDsaIT search POSITIVELY identified a glue
	// entry — never merely because the entry was absent. Meaningful only when
	// Probed, and it never travels alone: a glue pod is also Healthy false, so
	// the probe gauge reads 0 and the glue family says which of the two bad
	// states it is.
	Glue bool
}

// ClusterObservation is what one SlapdCluster reconcile pass observed: the
// cross-pod CSN comparison, per database, and the per-peer discovery result.
//
// The zero value renders no samples at all — not a set of zeros.
type ClusterObservation struct {
	Identity
	// Observed is when the pass ran. The zero time suppresses the timestamp
	// sample alone (the rest still render): a pass that produced readings but
	// recorded no time is a caller bug, and dropping only the freshness anchor
	// makes a staleness rule fire loudly instead of hiding it.
	Observed time.Time
	// Databases holds one entry per database the pass attempted. A database
	// absent here was not attempted.
	Databases []DatabaseCSN
	// Peers holds one entry per configured external peer.
	Peers []PeerObservation
}

// DatabaseObservation is what one SlapdDatabase reconcile pass observed: the
// per-pod suffix probes and whether syncrepl convergence is being withheld.
//
// The zero value renders no samples at all.
type DatabaseObservation struct {
	Identity
	// Database is the SlapdDatabase name; it labels every sample this render
	// produces, the timestamp included.
	Database string
	// Observed is when the pass ran; the zero time suppresses the timestamp
	// sample alone, as on ClusterObservation.
	Observed time.Time
	// Pods holds one entry per pod the pass considered, read-write and
	// read-only alike — the `role` label is what tells them apart, so an RO
	// pod is never dropped and never mistaken for an RW one.
	Pods []SuffixProbe
}

// RenderCluster renders one SlapdCluster pass.
//
// Returns the samples in a deterministic order — by metric name, then by the
// sample's rendered label set. Ordering is part of the contract: it costs one
// sort and it is what lets a test, a golden file or a diff name an expected
// sample set exactly.
//
// Absence rules, all of them ADR-033 decision 5:
//
//	MetricClusterObservationTimestamp — omitted when Observed is the zero time.
//	MetricCSNSpread                   — omitted when Spread is nil, and gated
//	                                    on NOTHING ELSE. One authority: a
//	                                    second gate on PairsRead would make
//	                                    this render silently drop a
//	                                    measurement its caller believed it had,
//	                                    which is the same silent loss decision
//	                                    5 forbids, pointing the other way. That
//	                                    a spread implies two readings is the
//	                                    producer's invariant, asserted where it
//	                                    is computed.
//	MetricCSNPairsRead/Expected       — one each per attempted database, always.
//	MetricPeerProvidersDiscovered     — omitted for a peer with UsesDiscovery
//	                                    false; there is no discovery result to
//	                                    report, not a result of zero. Omitted
//	                                    too when DiscoveryRan is false: a
//	                                    discovery that could not be ASKED is
//	                                    unreadable evidence, and Surface 1
//	                                    already reports that peer Unreachable.
//	                                    Emitted as 0 only for a discovery peer
//	                                    that ran and found nothing: there, the
//	                                    attempt is the observation.
func RenderCluster(obs ClusterObservation) []Sample {
	samples := make([]Sample, 0, 1+3*len(obs.Databases)+len(obs.Peers))

	if !obs.Observed.IsZero() {
		samples = append(samples, Sample{
			Name:   MetricClusterObservationTimestamp,
			Labels: sampleLabels(obs.Identity),
			Value:  float64(obs.Observed.Unix()),
		})
	}

	for _, db := range obs.Databases {
		dim := labelPair{LabelDatabase, db.Database}
		samples = append(samples,
			Sample{
				Name:   MetricCSNPairsExpected,
				Labels: sampleLabels(obs.Identity, dim),
				Value:  float64(db.PairsExpected),
			},
			Sample{
				Name:   MetricCSNPairsRead,
				Labels: sampleLabels(obs.Identity, dim),
				Value:  float64(db.PairsRead),
			},
		)
		// The pointer is the whole gate, and nothing else is consulted — not
		// PairsRead, which is where the hole was. A second gate would re-create
		// two authorities that have to agree, and the measured 0 it dropped
		// would be as silent as the unmeasured 0 it published. The producer's
		// invariant (a spread exists exactly when two or more pods were read)
		// is asserted where it is computed, which is the only place that can
		// know it.
		if db.Spread != nil {
			samples = append(samples, Sample{
				Name:   MetricCSNSpread,
				Labels: sampleLabels(obs.Identity, dim),
				Value:  db.Spread.Seconds(),
			})
		}
	}

	for _, peer := range obs.Peers {
		// Two different absences, one rule: there is no discovery RESULT here,
		// and a 0 would be read as one.
		//
		//	!UsesDiscovery — a `uri` or `podAddresses` peer never discovers, so
		//	                 0 would put an outage-shaped reading on a static
		//	                 configuration working exactly as declared.
		//	!DiscoveryRan  — discovery could not be ASKED (rotated kubeconfig,
		//	                 unparseable kubeconfig, no network mode, remote API
		//	                 down). 0 would send a reader to a remote site that
		//	                 is fine, for a fault that is entirely local. The
		//	                 failure is not lost: it leaves no addresses, so the
		//	                 peer reads Unreachable and Surface 1 publishes it.
		if !peer.UsesDiscovery || !peer.DiscoveryRan {
			continue
		}
		samples = append(samples, Sample{
			Name:   MetricPeerProvidersDiscovered,
			Labels: sampleLabels(obs.Identity, labelPair{LabelPeer, peer.Peer}),
			Value:  float64(peer.ProvidersDiscovered),
		})
	}

	return sortSamples(samples)
}

// RenderDatabase renders one SlapdDatabase pass.
//
// Same ordering contract as RenderCluster. Every sample carries the `database`
// label, MetricDatabaseObservationTimestamp included — a name of its own
// rather than the cluster anchor's, so neither family's label shape varies.
//
// Absence rules:
//
//	MetricDatabaseObservationTimestamp — omitted when Observed is the zero time.
//	MetricSuffixProbeOK                — omitted for a pod with Probed false.
//	                                     Emitted as 0 for a pod that WAS probed
//	                                     and came back unhealthy: here 0 is
//	                                     evidence, not silence. Always carries
//	                                     `role`, so no rule has to guess which
//	                                     fleet a pod belongs to.
//	MetricSuffixGlue                   — emitted as 1 only for a pod probed to
//	                                     a positively identified glue entry;
//	                                     presence is the signal and there is no
//	                                     0 counterpart. A missing suffix entry
//	                                     is NOT glue and emits nothing here —
//	                                     it shows up as the probe gauge's 0.
//	                                     Carries `role` like the probe gauge,
//	                                     because it is about the same pod.
func RenderDatabase(obs DatabaseObservation) []Sample {
	samples := make([]Sample, 0, 2+len(obs.Pods))
	db := labelPair{LabelDatabase, obs.Database}

	if !obs.Observed.IsZero() {
		samples = append(samples, Sample{
			Name:   MetricDatabaseObservationTimestamp,
			Labels: sampleLabels(obs.Identity, db),
			Value:  float64(obs.Observed.Unix()),
		})
	}

	for _, probe := range obs.Pods {
		// Probed is read before anything else and the rest of the verdict is
		// never consulted without it: an unprobed pod's Healthy and Glue are
		// both meaningless, and reading either first would publish a claim
		// nobody made — "we could not look" becoming "this pod is glued" being
		// the most consequential version of that mistake on this surface.
		if !probe.Probed {
			continue
		}

		// `role` identifies the pod on BOTH pod-scoped families, so that "glue
		// on a read-write pod" is a selector rather than a join. It grades
		// nothing: on the probe gauge it buys the read-only case a longer fuse
		// (an RO initial sync is legitimately missing its suffix entry), and on
		// the glue family it buys nothing at all, because a glue never heals
		// whichever fleet the pod belongs to.
		role := labelPair{LabelRole, RoleReadWrite}
		if probe.ReadOnly {
			role = labelPair{LabelRole, RoleReadOnly}
		}
		pod := labelPair{LabelPod, probe.Pod}

		value := 0.0
		if probe.Healthy {
			value = 1
		}
		samples = append(samples, Sample{
			Name:   MetricSuffixProbeOK,
			Labels: sampleLabels(obs.Identity, db, pod, role),
			Value:  value,
		})

		// Glue rides alongside the 0, never instead of it: the gauge carries
		// the verdict and this family says which of the two bad states it is. A
		// missing entry emits nothing here — it heals on the next sync, and an
		// alert that could not tell it from a glue would have to be written for
		// the weaker of the two.
		if probe.Glue {
			samples = append(samples, Sample{
				Name:   MetricSuffixGlue,
				Labels: sampleLabels(obs.Identity, db, pod, role),
				Value:  1,
			})
		}
	}

	return sortSamples(samples)
}

// labelPair is one dimension of one metric family: the extra label a family
// carries on top of the identity join key. A named pair rather than a variadic
// string list, so a key cannot silently land in a value's position.
type labelPair struct {
	name  string
	value string
}

// sampleLabels builds one sample's label set: the identity every family
// carries, plus that family's own dimensions.
//
// A fresh map per sample, deliberately. Sharing one between the samples of a
// database would make the returned slice an alias graph, and a caller adding a
// label to one sample would silently add it to three.
//
// `mesh` is always present and may be the empty string — see the package
// comment. `site` is never here; monitoring is site-local and the Prometheus
// is the site.
func sampleLabels(id Identity, extra ...labelPair) map[string]string {
	labels := make(map[string]string, 3+len(extra))
	labels[LabelNamespace] = id.Namespace
	labels[LabelCluster] = id.Cluster
	labels[LabelMesh] = id.Mesh
	for _, e := range extra {
		labels[e.name] = e.value
	}
	return labels
}

// sortSamples puts a render's output in the order the contract promises: by
// metric name, then by the rendered label set.
//
// Sorting on the rendered form is the same order, because String writes the
// name first and its labels sorted — and it is the form the contract is stated
// in, so the two cannot drift. It holds as long as no metric name is a prefix
// of another, which the documented families are not.
func sortSamples(samples []Sample) []Sample {
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].String() < samples[j].String()
	})
	return samples
}
