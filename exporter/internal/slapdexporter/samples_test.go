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

package slapdexporter

import (
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// typedValue is a sample's value together with its family type.
type typedValue struct {
	value float64
	typ   prometheus.ValueType
}

// typed indexes samples by identity with value and type, failing on a
// duplicate identity.
func typed(t *testing.T, samples []Sample) map[string]typedValue {
	t.Helper()
	out := make(map[string]typedValue, len(samples))
	for _, s := range samples {
		k := key(s.Name, s.Labels)
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate sample %s", k)
		}
		out[k] = typedValue{s.Value, s.Type}
	}
	return out
}

// startTime27 is cn=Start,cn=Time's monitorTimestamp in the 2.7.1 fixture,
// 20261008151855Z, as Unix seconds (date -u -d 2026-10-08T15:18:55Z +%s).
const startTime27 = 1791472735

// TestSamplesFromEntriesCuratedContract is ADR-033 decision 4's table, row by
// row, against the recorded 2.7.1 dump: every sample the dump yields — name,
// labels, value and type — and nothing else. The values are read off the
// fixture by hand, so the table and this test cannot share a mistake.
func TestSamplesFromEntriesCuratedContract(t *testing.T) {
	want := map[string]typedValue{
		bare(famConnsTotal):     {8, prometheus.CounterValue},
		bare(famConnsCurrent):   {1, prometheus.GaugeValue},
		bare(famMaxFDs):         {524288, prometheus.GaugeValue},
		bare(famSentBytes):      {19858, prometheus.CounterValue},
		bare(famSentPDUs):       {56, prometheus.CounterValue},
		bare(famSentEntries):    {39, prometheus.CounterValue},
		bare(famSentReferrals):  {0, prometheus.CounterValue},
		bare(famThreadsOpen):    {2, prometheus.GaugeValue},
		bare(famThreadsActive):  {1, prometheus.GaugeValue},
		bare(famThreadsPending): {0, prometheus.GaugeValue},
		bare(famThreadsStart):   {0, prometheus.GaugeValue},
		bare(famThreadsBack):    {1, prometheus.GaugeValue},
		bare(famThreadsMax):     {16, prometheus.GaugeValue},
		bare(famThreadsMaxPend): {0, prometheus.GaugeValue},
		key(famWaiters, map[string]string{"direction": "read"}):  {1, prometheus.GaugeValue},
		key(famWaiters, map[string]string{"direction": "write"}): {0, prometheus.GaugeValue},
		bare(famStartTime): {startTime27, prometheus.GaugeValue},
		// openldap_mdb_* is unchanged by the curation: gauges, by suffix.
		suffix("openldap_mdb_pages_max", statsSuffix):       {2560, prometheus.GaugeValue},
		suffix("openldap_mdb_pages_used", statsSuffix):      {20, prometheus.GaugeValue},
		suffix("openldap_mdb_pages_free", statsSuffix):      {12, prometheus.GaugeValue},
		suffix("openldap_mdb_readers_max", statsSuffix):     {126, prometheus.GaugeValue},
		suffix("openldap_mdb_readers_used", statsSuffix):    {1, prometheus.GaugeValue},
		suffix("openldap_mdb_entries", statsSuffix):         {3, prometheus.GaugeValue},
		suffix("openldap_mdb_page_size_bytes", statsSuffix): {4096, prometheus.GaugeValue},
	}
	for _, o := range []struct {
		op                   string
		initiated, completed float64
	}{
		{"bind", 8, 8}, {"unbind", 7, 7}, {"search", 3, 2}, {"compare", 0, 0}, {"modify", 0, 0},
		{"modrdn", 0, 0}, {"add", 8, 8}, {"delete", 0, 0}, {"abandon", 0, 0}, {"extended", 0, 0},
	} {
		want[op(famOpsInitiated, o.op)] = typedValue{o.initiated, prometheus.CounterValue}
		want[op(famOpsCompleted, o.op)] = typedValue{o.completed, prometheus.CounterValue}
	}

	got := typed(t, SamplesFromEntries(loadLDIF(t, fixtureMonitor27)))
	for _, k := range slices.Sorted(maps.Keys(want)) {
		switch g, ok := got[k]; {
		case !ok:
			t.Errorf("missing %s (want %v %v)", k, want[k].value, want[k].typ)
		case g != want[k]:
			t.Errorf("%s = %v %v, want %v %v", k, g.value, g.typ, want[k].value, want[k].typ)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[k]; !ok {
			t.Errorf("extra %s = %v — not in ADR-033's table", k, got[k].value)
		}
	}
}

// TestSamplesFromEntriesSameContractOn26: the -ol26 image's slapd yields the
// same identities and types as 2.7.1, less openldap_mdb_page_size_bytes —
// 2.6 has no olmMDBPageSize, and it is absent, not 0.
func TestSamplesFromEntriesSameContractOn26(t *testing.T) {
	want := map[string]prometheus.ValueType{}
	for k, v := range typed(t, SamplesFromEntries(loadLDIF(t, fixtureMonitor27))) {
		want[k] = v.typ
	}
	delete(want, suffix("openldap_mdb_page_size_bytes", statsSuffix))
	got := map[string]prometheus.ValueType{}
	for k, v := range typed(t, SamplesFromEntries(loadLDIF(t, fixtureMonitor26))) {
		got[k] = v.typ
	}
	if len(want) == 0 || !maps.Equal(got, want) {
		t.Errorf("2.6 identities = %v\nwant (2.7.1 less page size) %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}

// notMapped is every numeric cn=Monitor value the exporter deliberately does
// not export, as (entry DN, attribute) path.Match patterns. The first block is
// ADR-033 decision 4's "Not mapped" list. String-valued entries (cn=State,
// cn=Runqueue, cn=Tasklist, a database's backend name) are on that list too but
// need no row here: this test only asks about numeric values.
var notMapped = []struct{ dn, attr, why string }{
	{"cn=Operations,cn=Monitor", "monitorOpInitiated", "the aggregate is the sum over op"},
	{"cn=Operations,cn=Monitor", "monitorOpCompleted", "the aggregate is the sum over op"},
	{"cn=Uptime,cn=Time,cn=Monitor", "monitoredInfo", "time() - openldap_start_time_seconds"},
	{"cn=Current,cn=Time,cn=Monitor", "monitorTimestamp", "slapd's clock at read time, not a quantity"},

	// Beyond the ADR's list (found by this test against the recordings; see
	// the report to the ADR): none is read by the collector's search except the
	// listener counter.
	{"cn=Connection *,cn=Connections,cn=Monitor", "*",
		"one monitorConnection entry per open connection; the search filter excludes them"},
	{"cn=Log,cn=Monitor", "monitorLogLevel", "a writable log-level setting, not a measurement"},
	{"cn=Log,cn=Monitor", "monitorDebugLevel", "a writable debug-level setting, not a measurement"},
	{"cn=Listener *,cn=Listeners,cn=Monitor", "monitorTotalListenerConnections",
		"2.7+: connections accepted per listener; summed, openldap_connections_total"},
}

func isNotMapped(dn, attr string) bool {
	for _, n := range notMapped {
		dm, _ := path.Match(strings.ToLower(n.dn), strings.ToLower(dn))
		am, _ := path.Match(strings.ToLower(n.attr), strings.ToLower(attr))
		if dm && am {
			return true
		}
	}
	return false
}

// isNumericValue: what counts as a numeric cn=Monitor value — anything that
// parses as a number, and a monitorTimestamp (a point in time).
func isNumericValue(attr, v string) bool {
	if strings.EqualFold(attr, "monitorTimestamp") {
		return true
	}
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// mapsValue asks the implementation, not a copy of its table, whether one
// value is exported: it hands SamplesFromEntries the entry with only that
// attribute (plus objectClass and namingContexts, which select and label).
func mapsValue(e *ldap.Entry, attr *ldap.EntryAttribute) bool {
	probe := &ldap.Entry{DN: e.DN, Attributes: []*ldap.EntryAttribute{attr}}
	for _, keep := range []string{"objectClass", "namingContexts"} {
		if a := attrOf(e, keep); a != nil && !strings.EqualFold(a.Name, attr.Name) {
			probe.Attributes = append(probe.Attributes, a)
		}
	}
	return len(SamplesFromEntries([]*ldap.Entry{probe})) > 0
}

// TestEveryNumericMonitorValueIsMappedOrListed is ADR-033 decision 4's
// coverage test: in the recorded dump of every slapd we ship, each numeric
// value is either exported or on notMapped — never both, never neither. A new
// slapd release that adds or drops an entry turns this red, naming it.
func TestEveryNumericMonitorValueIsMappedOrListed(t *testing.T) {
	used := make([]bool, len(notMapped))
	for _, fx := range []string{fixtureMonitor27, fixtureMonitor26} {
		t.Run(fx, func(t *testing.T) {
			var numeric int
			for _, e := range loadLDIF(t, fx) {
				for _, a := range e.Attributes {
					if !slices.ContainsFunc(a.Values, func(v string) bool { return isNumericValue(a.Name, v) }) {
						continue
					}
					numeric++
					mapped, listed := mapsValue(e, a), isNotMapped(e.DN, a.Name)
					switch {
					case !mapped && !listed:
						t.Errorf("%s %s: %v is numeric, neither mapped nor on the not-mapped list", e.DN, a.Name, a.Values)
					case mapped && listed:
						t.Errorf("%s %s: mapped AND on the not-mapped list", e.DN, a.Name)
					}
					for i, n := range notMapped {
						dm, _ := path.Match(strings.ToLower(n.dn), strings.ToLower(e.DN))
						am, _ := path.Match(strings.ToLower(n.attr), strings.ToLower(a.Name))
						used[i] = used[i] || (dm && am)
					}
				}
			}
			if numeric == 0 {
				t.Fatal("no numeric values in the dump; the coverage check checked nothing")
			}
		})
	}
	for i, n := range notMapped {
		if !used[i] {
			t.Errorf("not-mapped row %s %s matches no value in any recording: stale", n.dn, n.attr)
		}
	}
}

// TestSamplesFromEntriesSkipsWhatIsNotANumber: a mapped entry whose value is
// not a number — or not a generalizedTime, for the start time — or is missing,
// yields no sample: absent, never 0 (ADR-033 decision 5). It costs that one
// series and no other.
func TestSamplesFromEntriesSkipsWhatIsNotANumber(t *testing.T) {
	entries := cloneEntries(loadLDIF(t, fixtureMonitor27))
	setAttr(findEntry(t, entries, "cn=Max,cn=Threads,cn=Monitor"), "monitoredInfo", []string{"unknown"})
	setAttr(findEntry(t, entries, "cn=Start,cn=Time,cn=Monitor"), "monitorTimestamp", []string{"yesterday"})
	setAttr(findEntry(t, entries, "cn=Bind,cn=Operations,cn=Monitor"), "monitorOpCompleted", nil)
	got := byKey(t, SamplesFromEntries(entries))
	for _, k := range []string{bare(famThreadsMax), bare(famStartTime), op(famOpsCompleted, "bind")} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %v from a value that is not a reading; want absent", k, v)
		}
	}
	for k, want := range map[string]float64{
		bare(famThreadsOpen):          2,
		op(famOpsInitiated, "bind"):   8,
		op(famOpsCompleted, "unbind"): 7,
		bare(famConnsTotal):           8,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present=%v), want %v: a sibling's bad value must not cost this series", k, v, ok, want)
		}
	}
}

// TestSamplesFromEntriesMatchesDNsCaseInsensitively: DNs compare without case
// in LDAP, and slapd returns what is stored. Label values are lowercase
// whatever the DN's spelling.
func TestSamplesFromEntriesMatchesDNsCaseInsensitively(t *testing.T) {
	e := &ldap.Entry{DN: "CN=Search,CN=OPERATIONS,cn=monitor"}
	setAttr(e, "objectClass", []string{"monitorOperation"})
	setAttr(e, "monitorOpCompleted", []string{"5"})
	w := &ldap.Entry{DN: "cn=READ,cn=waiters,cn=Monitor"}
	setAttr(w, "objectClass", []string{"monitorCounterObject"})
	setAttr(w, "monitorCounter", []string{"3"})
	got := byKey(t, SamplesFromEntries([]*ldap.Entry{e, w}))
	want := map[string]float64{
		op(famOpsCompleted, "search"):                           5,
		key(famWaiters, map[string]string{"direction": "read"}): 3,
	}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSamplesFromEntriesDatabaseWithoutStatisticsIsAbsentNotZero: a database
// opened before the monitor database carries no olmMDB* (ADR-033 amendment,
// "create order"). It gets no openldap_mdb_* series — not zeros, which would
// read as an empty, healthy database. Nor do cn=config, the frontend or the
// monitor database itself. Exactly one suffix in the fixture has statistics.
func TestSamplesFromEntriesDatabaseWithoutStatisticsIsAbsentNotZero(t *testing.T) {
	samples := SamplesFromEntries(loadLDIF(t, fixtureMonitor27))
	suffixes := map[string]int{}
	for _, s := range samples {
		if isMDBFamily(s.Name) {
			suffixes[s.Labels["suffix"]]++
		}
	}
	if want := map[string]int{statsSuffix: len(mdbFamilies)}; !maps.Equal(suffixes, want) {
		t.Errorf("openldap_mdb_* series per suffix = %v, want %v (and nothing for %q)", suffixes, want, noStatsSuffix)
	}
	got := byKey(t, samples)
	for _, f := range mdbFamilies {
		if v, ok := got[suffix(f, noStatsSuffix)]; ok {
			t.Errorf("%s = %v for a database that reports no statistics", suffix(f, noStatsSuffix), v)
		}
	}
}

// TestSamplesFromEntriesPageSizeAbsentOn26: 2.6 has no olmMDBPageSize. The
// other six are there; page size is absent, not 0.
func TestSamplesFromEntriesPageSizeAbsentOn26(t *testing.T) {
	got := byKey(t, filterSamples(SamplesFromEntries(loadLDIF(t, fixtureMonitor26)),
		func(s Sample) bool { return isMDBFamily(s.Name) }))
	var names []string
	for _, attr := range slices.Sorted(maps.Keys(mdbFamilies)) {
		if attr != "olmMDBPageSize" {
			names = append(names, mdbFamilies[attr])
		}
	}
	for _, n := range names {
		if _, ok := got[suffix(n, statsSuffix)]; !ok {
			t.Errorf("2.6: %s missing", suffix(n, statsSuffix))
		}
	}
	if v, ok := got[suffix("openldap_mdb_page_size_bytes", statsSuffix)]; ok {
		t.Errorf("2.6: openldap_mdb_page_size_bytes present (= %v); slapd 2.6 has no olmMDBPageSize", v)
	}
	if len(got) != len(names) {
		t.Errorf("2.6: %d mdb samples, want exactly %d: %v", len(got), len(names), slices.Sorted(maps.Keys(got)))
	}
}

// TestSamplesFromEntriesUnparseableMDBValueIsAbsent: one bad attribute costs
// one series, not the database's other six, and is never a 0.
func TestSamplesFromEntriesUnparseableMDBValueIsAbsent(t *testing.T) {
	entries := cloneEntries(loadLDIF(t, fixtureMonitor27))
	setAttr(findEntry(t, entries, statsDBDN), "olmMDBPagesUsed", []string{"not-a-number"})
	got := byKey(t, SamplesFromEntries(entries))
	if v, ok := got[suffix("openldap_mdb_pages_used", statsSuffix)]; ok {
		t.Errorf("openldap_mdb_pages_used present (= %v) from an unparseable value", v)
	}
	if v, ok := got[suffix("openldap_mdb_pages_max", statsSuffix)]; !ok || v != 2560 {
		t.Errorf("openldap_mdb_pages_max = %v (present=%v): a sibling's bad value must not cost this series", v, ok)
	}
}

// TestSamplesFromEntriesLabelsKnowSlapdNotSlaptain: a family carries only the
// label that means something to slapd — op, direction or suffix — or none. No
// cluster, pod or namespace: that context is the scrape target's, attached by
// the PodMonitor (ADR-033 amendment 2026-10-08: "the exporter knows slapd,
// never slaptain") — and no site, which no surface carries (amendment
// 2026-10-09: monitoring is site-local, the Prometheus is the site). And no dn.
func TestSamplesFromEntriesLabelsKnowSlapdNotSlaptain(t *testing.T) {
	samples := SamplesFromEntries(loadLDIF(t, fixtureMonitor27))
	if len(samples) == 0 {
		t.Fatal("no samples at all from the recorded dump")
	}
	for _, s := range samples {
		var want []string
		switch {
		case isMDBFamily(s.Name):
			want = []string{"suffix"}
		case s.Name == famOpsInitiated || s.Name == famOpsCompleted:
			want = []string{"op"}
		case s.Name == famWaiters:
			want = []string{"direction"}
		}
		if got := slices.Sorted(maps.Keys(s.Labels)); !slices.Equal(got, want) {
			t.Errorf("%s labels %v, want %v", key(s.Name, s.Labels), got, want)
		}
	}
}
