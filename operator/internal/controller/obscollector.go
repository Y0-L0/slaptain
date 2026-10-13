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

package controller

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/obsmetrics"
)

// This file is the ONLY place in the operator that imports
// prometheus/client_golang, and it is the whole of the binding: internal/
// obsmetrics renders []Sample out of plain structs and stays stdlib-only, so
// the decisions this surface turns on (ADR-033 decision 5 — unreadable evidence
// is absent, never 0) are testable without a registry, a desc or a scrape.
//
// Everything here is adapter: it reads two snapshots, runs the matching render,
// and converts each Sample into a const metric. No decision is taken.

// obsMetricHelp is the help string for every family Surface 2 exports.
//
// A map keyed on the metric name, because Prometheus requires one help string
// per metric name within one scrape and errors the WHOLE gather if two samples
// of a name disagree. Deriving it per sample is how that happens.
var obsMetricHelp = map[string]string{
	obsmetrics.MetricClusterObservationTimestamp:  "Unix time of the SlapdCluster reconcile pass that produced this cluster's Surface 2 samples. Everything else here is only as fresh as this.",
	obsmetrics.MetricDatabaseObservationTimestamp: "Unix time of the SlapdDatabase reconcile pass that produced this database's Surface 2 samples.",
	obsmetrics.MetricCSNSpread:                    "Measured contextCSN spread across the read-write pods that answered for one database, in seconds; 0 means two or more pods answered and agree. Absent whenever no spread was measured, which includes fewer than two readable pods.",
	obsmetrics.MetricCSNPairsRead:                 "(pod, database) pairs that yielded a readable contextCSN this pass.",
	obsmetrics.MetricCSNPairsExpected:             "(pod, database) pairs this pass set out to read for one database.",
	obsmetrics.MetricPeerProvidersDiscovered:      "Remote pods peer discovery found for one external peer; 0 means discovery ran and resolved to nothing. Absent for a statically configured peer and for one whose discovery could not be asked at all, neither of which is a result of zero.",
	obsmetrics.MetricSuffixProbeOK:                "ADR-025 suffix-entry probe for one pod and database: 1 healthy, 0 glue or missing. Absent for a pod that could not be probed. The role label separates read-write pods from read-only consumers, whose initial sync is legitimately transient.",
	obsmetrics.MetricSuffixGlue:                   "This pod's suffix entry is a glue entry (ADR-025): hidden from ordinary searches, backups from it unrestorable, never self-healing. Always 1; presence is the signal.",
}

// errDuplicateSample is what a dropped duplicate is logged WITH, so the drop
// reaches error-based alerting rather than sitting at info on the root logger.
// It is always a bug in the mapping — the renders sort and do not produce
// duplicates — and a bug nobody is told about is one that lives for a release.
var errDuplicateSample = errors.New("duplicate Surface 2 sample")

// ObservationCollector exports Surface 2 (ADR-033 decision 3) from what the
// reconcilers last observed.
//
// It reads the stores and renders; it never queries anything. A scrape that
// went and looked would defeat decision 3's "no pod-local counter" rule and
// would put an LDAP dial on the critical path of a metrics endpoint, so Collect
// does no I/O and takes no lock the reconcilers hold for longer than a map
// read.
type ObservationCollector struct {
	// Clusters and Databases are the stores the two reconcilers record into.
	// Injected, so a collector is constructible in a test against stores it
	// filled itself.
	Clusters  *obsmetrics.Store[obsmetrics.ClusterObservation]
	Databases *obsmetrics.Store[obsmetrics.DatabaseObservation]
}

var _ prometheus.Collector = (*ObservationCollector)(nil)

// Describe sends nothing, which makes this an UNCHECKED collector in
// client_golang's terms, deliberately.
//
// A checked collector has to declare every descriptor up front, and the label
// sets here are not knowable up front: `database`, `peer`, `pod` and `reason`
// values come from the clusters this operator happens to manage. Declaring a
// desc per family would still leave the registry comparing each emitted metric
// against it and erroring the scrape on any mismatch — trading a shape we
// cannot fix at registration time for a scrape that fails at runtime. An
// unchecked collector is checked where it matters instead: duplicate (name,
// labels) pairs are caught in Collect below.
func (c *ObservationCollector) Describe(chan<- *prometheus.Desc) {}

// Collect renders both stores' current snapshots.
//
// No I/O, no blocking on a reconcile: Snapshot takes a read lock, copies, and
// returns. The renders are pure.
func (c *ObservationCollector) Collect(ch chan<- prometheus.Metric) {
	log := logf.Log.WithName("obsmetrics")

	// One seen-set across BOTH renders. The two stores are keyed
	// independently, so a mapping bug could in principle produce the same
	// (name, labels) twice; the renders themselves sort and do not.
	seen := make(map[string]struct{})

	emit := func(samples []obsmetrics.Sample) {
		for _, s := range samples {
			key := s.Name + "|" + labelKey(s.Labels)
			if _, dup := seen[key]; dup {
				// Dropped rather than emitted, because a duplicate (name,
				// labels) pair makes Prometheus error the ENTIRE scrape: every
				// series would be lost, the freshness anchors included, and the
				// surface would go dark exactly when something is wrong with
				// it. Dropping costs one series and keeps the rest readable.
				//
				// Logged at ERROR and on every scrape, with no dedup cache: a
				// bug that says so twice a minute is a bug that gets noticed,
				// and a cache would be one more piece of state to keep true.
				// The realistic cause is two external peers sharing a `name`
				// (ExternalPeers has no listMapKey), which is a spec defect
				// this is the loudest report of.
				log.Error(errDuplicateSample,
					"dropped a duplicate Surface 2 sample; a scrape carrying it would fail entirely",
					"metric", s.Name, "labels", s.Labels)
				continue
			}
			seen[key] = struct{}{}

			metric, err := prometheus.NewConstMetric(
				prometheus.NewDesc(s.Name, obsMetricHelp[s.Name], nil, s.Labels),
				prometheus.GaugeValue, s.Value)
			if err != nil {
				// NewConstMetric rather than MustNewConstMetric: this runs on
				// the scrape handler's goroutine, and an operator taken down by
				// its own metrics endpoint is a worse outcome than a missing
				// series. Unreachable with the constant names and label names
				// obsmetrics uses — which is the reason to log it rather than
				// to handle it.
				log.Error(err, "could not build a Surface 2 metric", "metric", s.Name, "labels", s.Labels)
				continue
			}
			ch <- metric
		}
	}

	if c.Clusters != nil {
		for _, obs := range c.Clusters.Snapshot() {
			emit(obsmetrics.RenderCluster(obs))
		}
	}
	if c.Databases != nil {
		for _, obs := range c.Databases.Snapshot() {
			emit(obsmetrics.RenderDatabase(obs))
		}
	}
}

// labelKey is a label set's identity for duplicate detection: the sample's
// rendered label list without its value, since two samples of one series
// carrying different numbers are the duplicate, not two different series.
func labelKey(labels map[string]string) string {
	return obsmetrics.Sample{Labels: labels}.String()
}
