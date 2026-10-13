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

package obsmetrics

import (
	"slices"
	"strings"
	"testing"
)

// This file exists because of a miss, not a theory.
//
// slaptain_syncrepl_withheld shipped as a Surface 2 family although the
// quantity is a SlapdDatabase status condition, which ADR-033 decision 2 had
// already given to Surface 1 — six live
// slaptain_database_condition{condition="SyncreplStanzasReady"} series on the
// mesh say so. Decision 1 is the rule the whole ADR exists to establish ("a
// metric belongs to the vantage point that observes it DIRECTLY"), and nothing
// checked it. The ADR contradicted itself and the contradiction was reviewable
// only by a human reading two documents at once.
//
// What a test CAN do about that is narrow, and worth being honest about: it
// cannot read a new family's meaning. What it can do is make the question
// unavoidable — a family with no provenance row fails, and a row whose honest
// answer is "a CR status condition" fails by construction. The work is the
// author's; the test is what stops the work being skipped.

// surface1Families is what kube-state-metrics exports from CR status, as
// monitoring/kube-state-metrics-values.yaml declares it (ADR-033 decision 2).
// Hand-maintained: the config is YAML in another tree and this package may not
// read files. If a name is added there, add it here.
var surface1Families = []string{
	"slaptain_cluster_info",
	"slaptain_cluster_replicas_ready",
	"slaptain_cluster_replicas_desired",
	"slaptain_cluster_readonly_replicas_ready",
	"slaptain_cluster_readonly_replicas_desired",
	"slaptain_cluster_observed_generation",
	"slaptain_cluster_condition",
	"slaptain_cluster_condition_last_transition_seconds",
	"slaptain_cluster_peer_connected",
	"slaptain_cluster_peer_lag_seconds",
	"slaptain_cluster_peer_last_checked_seconds",
	"slaptain_database_info",
	"slaptain_database_condition",
	"slaptain_database_condition_last_transition_seconds",
}

// surface1Conditions is every condition type Surface 1 carries as a `condition`
// LABEL VALUE on the two condition families above. They are the trap: the
// quantity is exported, but under a name that shares no substring with the
// Surface 2 family someone would invent for it — "SyncreplStanzasReady" against
// "slaptain_syncrepl_withheld" — so no name-matching rule can find them.
// Naming them here is what makes the provenance rows below answerable.
var surface1Conditions = []string{
	"ReplicationConverged",
	"DataPresent",
	"PodsResponsive",
	"TLSReady",
	"MeshResolved",
	"SyncreplStanzasReady",
	"SourceConverged",
	"SourceSuffixHealthy",
}

// provenance is one family's answer to decision 1: what does the operator hold
// that no other vantage point does?
type provenance struct {
	// Evidence names the thing the operator performed or discovered. It is
	// prose, and no test can judge it — but writing it is the step that was
	// skipped, and an empty one fails.
	Evidence string
	// FromCondition names the CR status condition this family is derived from,
	// where that is the honest answer. A non-empty value FAILS the test:
	// Surface 1 owns every condition, carries its reason as a label, and
	// carries the Unknown case a gauge cannot express.
	FromCondition string
}

// surface2Provenance is one row per family in Families. A family with no row
// fails; a row naming a condition fails. Both failures are the same question
// asked twice, which is the question nobody asked about syncrepl_withheld.
var surface2Provenance = map[string]provenance{
	MetricClusterObservationTimestamp: {
		Evidence: "when the operator's own pass ran — a fact about the observer, which " +
			"no observed object records anywhere",
	},
	MetricDatabaseObservationTimestamp: {
		Evidence: "the same, for the database pass",
	},
	MetricCSNSpread: {
		Evidence: "a comparison ACROSS pods the operator performed. status.conditions " +
			"keeps the boolean it reduces to (ReplicationConverged); the measured " +
			"quantity is nowhere in CR status",
	},
	MetricCSNPairsRead: {
		Evidence: "how many (pod, database) reads succeeded this pass — an operator-side " +
			"count that lived only as English inside a condition message",
	},
	MetricCSNPairsExpected: {
		Evidence: "how many the pass attempted; same vantage point, and the denominator " +
			"the one above is only meaningful against",
	},
	MetricPeerProvidersDiscovered: {
		Evidence: "the result of a query the operator made to a REMOTE cluster's API. " +
			"status.externalPeerStatuses[].discoveredProviders records it, but as a " +
			"LIST — kube-state-metrics exports its presence, never its length",
	},
	MetricSuffixProbeOK: {
		Evidence: "one pod's suffix-entry probe. DataPresent is the aggregate verdict " +
			"over pods and is Surface 1's; WHICH pod answered what is not in CR status",
	},
	MetricSuffixGlue: {
		Evidence: "which pod carries a glue suffix entry. DataPresent's GlueSuffix reason " +
			"says the database has one somewhere — the per-POD fact exists only here",
	},
}

// TestEveryFamilyDeclaresWhatObservesItDirectly is the decision-1 guard.
//
// Had it existed when slaptain_syncrepl_withheld was written, the row for it
// would have had to say either "the SyncreplStanzasReady condition" — which
// fails — or something untrue. That is the whole mechanism: it does not detect
// a violation, it refuses to let one be added silently.
func TestEveryFamilyDeclaresWhatObservesItDirectly(t *testing.T) {
	for _, name := range Families {
		p, declared := surface2Provenance[name]
		if !declared {
			t.Errorf("family %q has no provenance row. Before adding one, answer ADR-033 "+
				"decision 1: which vantage point observes this DIRECTLY? If the answer is "+
				"a CR status condition or anything else already in status, it is Surface "+
				"1's and must not be exported here", name)
			continue
		}
		if strings.TrimSpace(p.Evidence) == "" {
			t.Errorf("family %q declares no evidence; the row is the argument, not a "+
				"formality", name)
		}
		if p.FromCondition != "" {
			t.Errorf("family %q is derived from the %q condition, which Surface 1 already "+
				"exports as slaptain_{cluster,database}_condition — with its reason as a "+
				"label and with the Unknown case this surface cannot express (ADR-033 "+
				"decision 1)", name, p.FromCondition)
		}
	}

	// A stale row outlives the family it described and quietly re-blesses the
	// name if someone brings it back.
	for name := range surface2Provenance {
		if !slices.Contains(Families, name) {
			t.Errorf("provenance row for %q, which is not a family this package renders; "+
				"remove the row with the family", name)
		}
	}
}

// TestNoSurface2FamilyCollidesWithSurface1 is the cheap half: one name, one
// owner. A collision would merge two different quantities, measured at two
// vantage points with two label shapes, into one unqueryable series — and
// Prometheus would not complain, it would just interleave them.
func TestNoSurface2FamilyCollidesWithSurface1(t *testing.T) {
	owned := map[string]bool{}
	for _, name := range surface1Families {
		owned[name] = true
	}
	for _, name := range Families {
		if owned[name] {
			t.Errorf("family %q is exported by BOTH surfaces; decision 1 allows exactly one "+
				"overlap, `up`, and this is not it", name)
		}
	}
}

// TestNoFamilyIsNamedAfterASurface1Condition is the weakest of the three and is
// kept for what it documents rather than what it catches: every condition
// Surface 1 carries, in one list, next to the rule that they are not ours.
//
// It would NOT have caught syncrepl_withheld — "SyncreplStanzasReady" and
// "slaptain_syncrepl_withheld" share no snake-cased name — which is exactly why
// the provenance table above is the real guard and this one is a reading aid.
// Said plainly here so nobody mistakes it for coverage.
func TestNoFamilyIsNamedAfterASurface1Condition(t *testing.T) {
	for _, cond := range surface1Conditions {
		snake := strings.ToLower(cond)
		for _, name := range Families {
			bare := strings.TrimPrefix(name, "slaptain_")
			if strings.ReplaceAll(bare, "_", "") == snake {
				t.Errorf("family %q is the %q condition under another name; Surface 1 owns "+
					"it (ADR-033 decision 1)", name, cond)
			}
		}
	}
}
