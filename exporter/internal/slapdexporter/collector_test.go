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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// The collector behind the Conn seam. Every case drives a fake slapd through a
// scripted sequence of scrapes against ONE registry, because the failure modes
// ADR-033 decision 5 forbids — a cached last value, a counter reset, a series
// that lingers after its evidence went away — only exist across scrapes.

// healthyFake is a slapd that answers the recorded 2.7.1 dump.
func healthyFake(t *testing.T) *fakeSlapd {
	return &fakeSlapd{entries: loadLDIF(t, fixtureMonitor27)}
}

// TestCollectorReadsOnScrapeNotOnATimer pins collect-on-scrape (ADR-033
// amendment 2026-10-08): no connection before the first scrape, exactly one
// per scrape after.
func TestCollectorReadsOnScrapeNotOnATimer(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)

	// A ticker-driven implementation would have dialled by now; collect-on-scrape
	// cannot have. Only a tripwire — the per-scrape counts below are the pin.
	time.Sleep(50 * time.Millisecond)
	if d, _, _, _ := f.counts(); d != 0 {
		t.Fatalf("%d dials before any scrape; the exporter reads cn=Monitor only when scraped", d)
	}
	for i := 1; i <= 3; i++ {
		scrape(t, reg)
		if d, _, _, s := f.counts(); d != i || s != i {
			t.Fatalf("after %d scrapes: %d dials, %d searches; want %d of each", i, d, s, i)
		}
	}
}

// TestCollectorDialsTheConfiguredURI: the collector hands Options.URI to the
// DialFunc unchanged (normalisation is ParseFlags' job, once).
func TestCollectorDialsTheConfiguredURI(t *testing.T) {
	f := healthyFake(t)
	opts := externalOpts()
	opts.URI = remoteURI
	scrape(t, newScrapeTarget(t, opts, f))
	if !slices.Equal(f.uris, []string{opts.URI}) {
		t.Errorf("dialled %q, want [%q]", f.uris, opts.URI)
	}
}

// TestCollectorSearchesMonitorSubtreeWithOperationalAttributes: monitorCounter,
// monitoredInfo, monitorOp*, monitorTimestamp and olmMDB* are OPERATIONAL attributes; a
// search that does not request "+" gets none of them back from a real slapd
// (the fake would not notice — this assertion is what does).
func TestCollectorSearchesMonitorSubtreeWithOperationalAttributes(t *testing.T) {
	f := healthyFake(t)
	scrape(t, newScrapeTarget(t, externalOpts(), f))
	if len(f.searches) != 1 {
		t.Fatalf("%d searches in one scrape, want 1", len(f.searches))
	}
	req := f.searches[0]
	if req.BaseDN != "cn=Monitor" {
		t.Errorf("BaseDN = %q, want cn=Monitor", req.BaseDN)
	}
	if req.Scope != ldap.ScopeWholeSubtree {
		t.Errorf("Scope = %d, want ScopeWholeSubtree", req.Scope)
	}
	// Only the three classes the families read — per-database entries are
	// monitoredObject (+ the olmMDBDatabase auxiliary), so the mdb statistics
	// still match. Not (objectClass=*): every open connection is a
	// monitorConnection entry with ~25 attributes, which a busy slapd would
	// serialise on every scrape for nothing.
	if want := "(|(objectClass=monitoredObject)(objectClass=monitorCounterObject)(objectClass=monitorOperation))"; req.Filter != want {
		t.Errorf("Filter = %q\nwant     %q", req.Filter, want)
	}
	if !slices.Contains(req.Attributes, "+") {
		t.Errorf("Attributes = %q: must request \"+\" (all operational attributes)", req.Attributes)
	}
	if !slices.Contains(req.Attributes, "*") && !slices.Contains(req.Attributes, "objectClass") {
		t.Errorf("Attributes = %q: objectClass (a user attribute) is needed to tell the families apart", req.Attributes)
	}
}

// TestCollectorFilteredReadingYieldsEveryFamily: what slapd actually returns
// for the collector's filter (testdata/monitor-filtered-2.7.1.ldif, recorded
// with exactly that filter — no containers, no connections, no cn=Monitor
// itself) still yields every series the unfiltered dump does. Values differ:
// the two recordings are separate sessions; the identities may not.
func TestCollectorFilteredReadingYieldsEveryFamily(t *testing.T) {
	f := &fakeSlapd{entries: loadLDIF(t, fixtureMonitorFiltered27)}
	got := monitorKeys(t, scrape(t, newScrapeTarget(t, externalOpts(), f)))
	want := byKey(t, SamplesFromEntries(loadLDIF(t, fixtureMonitor27)))
	if len(want) == 0 {
		t.Fatal("the unfiltered mapping is empty; the comparison would be vacuous")
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("filtered reading lacks %s", k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("filtered reading has unexpected %s", k)
		}
	}
}

// TestCollectorInvalidLabelValueCostsOneSeries: slapd hands back whatever bytes
// a suffix holds, and a Prometheus label value must be valid UTF-8. Building that
// one metric fails; it must cost that one series — not a panic in Collect
// (MustNewConstMetric), which takes down the whole process and every scrape
// after it, and not the rest of this scrape.
func TestCollectorInvalidLabelValueCostsOneSeries(t *testing.T) {
	entries := cloneEntries(loadLDIF(t, fixtureMonitor27))
	// A database whose suffix is not valid UTF-8: the suffix label is taken
	// verbatim (an op or direction label is lowercased, which already replaces
	// invalid bytes).
	bad := &ldap.Entry{DN: "cn=Database 9,cn=Databases,cn=Monitor"}
	setAttr(bad, "objectClass", []string{"monitoredObject", "olmMDBDatabase"})
	setAttr(bad, "namingContexts", []string{"dc=bad" + string([]byte{0xff})})
	setAttr(bad, "olmMDBEntries", []string{"7"})
	entries = append(entries, bad)
	f := &fakeSlapd{entries: entries}

	scr := scrape(t, newScrapeTarget(t, externalOpts(), f))
	got := monitorKeys(t, scr)
	want := byKey(t, SamplesFromEntries(loadLDIF(t, fixtureMonitor27)))
	if len(want) == 0 || !maps.Equal(got, want) {
		t.Errorf("scrape with one invalid-UTF-8 DN carries %d series, want exactly the %d valid ones", len(got), len(want))
	}
	if up := byKey(t, samplesOf(scr))[bare(famUp)]; up != 1 {
		t.Errorf("%s = %v; the read succeeded, one unrepresentable series does not make it fail", famUp, up)
	}
}

// TestCollectorEmitsWhatTheMappingSays: a healthy scrape carries exactly
// SamplesFromEntries of what the search returned — the collector adds no
// cn=Monitor-derived series of its own and drops none.
func TestCollectorEmitsWhatTheMappingSays(t *testing.T) {
	f := healthyFake(t)
	got := monitorKeys(t, scrape(t, newScrapeTarget(t, externalOpts(), f)))
	want := byKey(t, SamplesFromEntries(f.entries))
	if len(got) == 0 || len(want) == 0 {
		t.Fatalf("healthy scrape yields %d cn=Monitor series, the mapping %d; both must be non-empty", len(got), len(want))
	}
	if !maps.Equal(got, want) {
		t.Errorf("scrape  = %v\nmapping = %v", got, want)
	}
	if v := got[suffix("openldap_mdb_entries", statsSuffix)]; v != 3 {
		t.Errorf("openldap_mdb_entries{suffix=%q} = %v, want 3", statsSuffix, v)
	}
}

// TestCollectorUpSaysWhetherThisScrapeRead: openldap_up is always present —
// 1 when this scrape's read succeeded, 0 when the dial, the bind or the search
// failed. It is the exporter's observation of its own attempt, so 0 is a
// reading (ADR-033 decision 4), and it follows each scrape, never latching.
func TestCollectorUpSaysWhetherThisScrapeRead(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)
	upOf := func() (float64, bool) {
		v, ok := byKey(t, samplesOf(scrape(t, reg)))[bare(famUp)]
		return v, ok
	}
	for _, step := range []struct {
		name string
		set  func(*fakeSlapd)
		want float64
	}{
		{"healthy", func(*fakeSlapd) {}, 1},
		{caseDialFails, func(f *fakeSlapd) { f.dialErr = errFake }, 0},
		{"recovered", func(f *fakeSlapd) { f.dialErr = nil }, 1},
		{caseBindFails, func(f *fakeSlapd) { f.bindErr = errFake }, 0},
		{caseSearchFails, func(f *fakeSlapd) { f.bindErr, f.searchErr = nil, errFake }, 0},
		{"partial search", func(f *fakeSlapd) { f.partial = true }, 0},
		{"recovered again", func(f *fakeSlapd) { f.searchErr, f.partial = nil, false }, 1},
	} {
		f.set(step.set)
		if v, ok := upOf(); !ok || v != step.want {
			t.Errorf("%s: %s = %v (present=%v), want %v", step.name, famUp, v, ok, step.want)
		}
	}
}

// TestCollectorFailedReadIsAbsentNeverTheLastValue — ADR-033 decision 5: a
// scrape that follows a good one, and whose read fails, carries NO
// cn=Monitor-derived series. Not the previous values, not zeros.
func TestCollectorFailedReadIsAbsentNeverTheLastValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*fakeSlapd)
	}{
		{caseDialFails, func(f *fakeSlapd) { f.dialErr = errFake }},
		{caseBindFails, func(f *fakeSlapd) { f.bindErr = errFake }},
		{caseSearchFails, func(f *fakeSlapd) { f.searchErr = errFake }},
		// go-ldap returns entries AND an error for e.g. a size limit hit
		// mid-search. A partial tree is not a reading: half the families
		// present and half absent would be read as a real change.
		{"search fails after returning entries", func(f *fakeSlapd) { f.searchErr, f.partial = errFake, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake(t)
			reg := newScrapeTarget(t, externalOpts(), f)
			if first := monitorKeys(t, scrape(t, reg)); len(first) == 0 {
				t.Fatal("first (healthy) scrape carried no cn=Monitor series")
			}
			f.set(tc.fail)
			scr := scrape(t, reg)
			if second := monitorKeys(t, scr); len(second) != 0 {
				t.Errorf("failed read still exported %d cn=Monitor series, e.g. %v", len(second), firstKeys(second, 3))
			}
			if up, ok := byKey(t, samplesOf(scr))[bare(famUp)]; !ok || up != 0 {
				t.Errorf("%s = %v (present=%v) after a failed read, want 0", famUp, up, ok)
			}
		})
	}
}

// TestCollectorRecoversAfterAFailedRead: the reverse sequence. A failure must
// not latch — the next good read is exported in full.
func TestCollectorRecoversAfterAFailedRead(t *testing.T) {
	f := healthyFake(t)
	f.searchErr = errFake
	reg := newScrapeTarget(t, externalOpts(), f)
	if first := monitorKeys(t, scrape(t, reg)); len(first) != 0 {
		t.Fatalf("failed first read exported %d series", len(first))
	}
	f.set(func(f *fakeSlapd) { f.searchErr = nil })
	got := monitorKeys(t, scrape(t, reg))
	want := byKey(t, SamplesFromEntries(f.entries))
	if len(got) == 0 || !maps.Equal(got, want) {
		t.Errorf("recovered scrape carries %d series, want the full %d", len(got), len(want))
	}
}

// TestCollectorValuesAreFreshEachScrape: a value that changed in slapd changes
// in the next scrape. Catches a collector that maps once and replays.
func TestCollectorValuesAreFreshEachScrape(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)
	k := bare(famThreadsMax)
	if v := monitorKeys(t, scrape(t, reg))[k]; v != 16 {
		t.Fatalf("first scrape %s = %v, want 16", k, v)
	}
	f.set(func(f *fakeSlapd) {
		f.entries = cloneEntries(f.entries)
		setAttr(findEntry(t, f.entries, "cn=Max,cn=Threads,cn=Monitor"), "monitoredInfo", []string{"32"})
	})
	if v := monitorKeys(t, scrape(t, reg))[k]; v != 32 {
		t.Errorf("second scrape %s = %v, want 32 (the value slapd now reports)", k, v)
	}
}

// TestCollectorSeriesWhoseEvidenceVanishedIsGone: a database that carried
// olmMDB* on one scrape and none on the next loses its openldap_mdb_* series —
// a gauge vector that is never reset would keep them.
func TestCollectorSeriesWhoseEvidenceVanishedIsGone(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)
	k := suffix("openldap_mdb_pages_used", statsSuffix)
	if _, ok := monitorKeys(t, scrape(t, reg))[k]; !ok {
		t.Fatalf("first scrape lacks %s", k)
	}
	f.set(func(f *fakeSlapd) {
		f.entries = cloneEntries(f.entries)
		removeMDBStats(findEntry(t, f.entries, statsDBDN))
	})
	for k2, v := range monitorKeys(t, scrape(t, reg)) {
		if isMDBFamilyKey(k2) {
			t.Errorf("second scrape still carries %s = %v; its database no longer reports statistics", k2, v)
		}
	}
}

func isMDBFamilyKey(k string) bool { return len(k) > 13 && k[:13] == "openldap_mdb_" }

func firstKeys(m map[string]float64, n int) []string {
	ks := slices.Sorted(maps.Keys(m))
	return ks[:min(n, len(ks))]
}

// TestCollectorCountsDialAndBindAttempts pins openldap_exporter_dials_total /
// openldap_exporter_binds_total — label result="ok"|"fail", accumulating across
// scrapes. A failed dial attempts no bind and no search.
func TestCollectorCountsDialAndBindAttempts(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)

	scrape(t, reg)
	scrape(t, reg)
	f.set(func(f *fakeSlapd) { f.bindErr = errFake })
	scrape(t, reg)
	f.set(func(f *fakeSlapd) { f.bindErr, f.dialErr = nil, errFake })
	last := scrape(t, reg)

	got := byKey(t, filterSamples(samplesOf(last), func(s Sample) bool { return s.Name == famDials || s.Name == famBinds }))
	want := map[string]float64{
		result(famDials, "ok"):   3,
		result(famDials, "fail"): 1,
		result(famBinds, "ok"):   2,
		result(famBinds, "fail"): 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("attempt counters = %v\nwant              %v", got, want)
	}
	if _, _, b, s := f.counts(); b != 3 || s != 2 {
		t.Errorf("%d binds, %d searches over 4 scrapes; want 3 binds (none after the failed dial) and 2 searches "+
			"(none after the failed bind or dial)", b, s)
	}
}

// TestCollectorBindModes: each --bind does what it says, once per scrape.
// Under none there is no bind attempt and so no openldap_exporter_binds_total
// series: no attempt is no evidence, not a 0.
func TestCollectorBindModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      Options
		wantBinds []string
		wantBind  bool // openldap_exporter_binds_total present
	}{
		{"external", externalOpts(), []string{"external"}, true},
		{"simple", Options{URI: DefaultURI, Bind: BindSimple, BindDN: monitorBindDN,
			BindPassword: testPassword, Listen: DefaultListen},
			[]string{"simple:cn=monitor,dc=example,dc=org:s3cret"}, true},
		{"none", Options{URI: DefaultURI, Bind: BindNone, Listen: DefaultListen}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake(t)
			got := scrape(t, newScrapeTarget(t, tc.opts, f))
			if !slices.Equal(f.binds, tc.wantBinds) {
				t.Errorf("binds = %q, want %q", f.binds, tc.wantBinds)
			}
			hasBind := slices.ContainsFunc(got, func(s scraped) bool { return s.Name == famBinds })
			if hasBind != tc.wantBind {
				t.Errorf("%s present = %v, want %v", famBinds, hasBind, tc.wantBind)
			}
			if len(monitorKeys(t, got)) == 0 {
				t.Error("no cn=Monitor series although the read succeeded")
			}
		})
	}
}

// TestCollectorClosesEveryConnection: one connection per scrape, closed in the
// same scrape whatever happened on it — a sidecar that leaks a connection per
// failed scrape exhausts slapd's descriptors on exactly the bad day.
func TestCollectorClosesEveryConnection(t *testing.T) {
	f := healthyFake(t)
	reg := newScrapeTarget(t, externalOpts(), f)
	scrape(t, reg)
	f.set(func(f *fakeSlapd) { f.bindErr = errFake })
	scrape(t, reg)
	f.set(func(f *fakeSlapd) { f.bindErr, f.searchErr = nil, errFake })
	scrape(t, reg)
	f.set(func(f *fakeSlapd) { f.searchErr, f.partial = errFake, true })
	scrape(t, reg)
	if d, c, _, _ := f.counts(); d != 4 || c != 4 {
		t.Errorf("%d dials, %d closes; every opened connection must be closed in its scrape", d, c)
	}
}

// TestCollectorFamilyTypes: ADR-033 decision 4's types, as the registry
// gathers them — every _total family a counter, everything else a gauge,
// openldap_up included. The registry holds only this collector, so the map is
// also the complete family set a healthy scrape carries.
func TestCollectorFamilyTypes(t *testing.T) {
	got := map[string]string{}
	for _, s := range scrape(t, newScrapeTarget(t, externalOpts(), healthyFake(t))) {
		got[s.Name] = s.Type
	}
	want := map[string]string{famUp: typeGauge, famDials: typeCounter, famBinds: typeCounter,
		famOpsInitiated: typeCounter, famOpsCompleted: typeCounter, famConnsTotal: typeCounter,
		famSentBytes: typeCounter, famSentPDUs: typeCounter, famSentEntries: typeCounter, famSentReferrals: typeCounter,
		famConnsCurrent: typeGauge, famMaxFDs: typeGauge, famThreadsOpen: typeGauge, famThreadsActive: typeGauge,
		famThreadsPending: typeGauge, famThreadsStart: typeGauge, famThreadsBack: typeGauge, famThreadsMax: typeGauge,
		famThreadsMaxPend: typeGauge, famWaiters: typeGauge, famStartTime: typeGauge}
	for _, f := range mdbFamilies {
		want[f] = typeGauge
	}
	if !maps.Equal(got, want) {
		t.Errorf("family types = %v\nwant           %v", got, want)
	}
}

// TestCollectorTimeoutBoundsAHungSlapd — Options.Timeout (default 5s, below
// Prometheus's 10s scrape timeout) bounds dial + bind + search of one scrape.
// ADR-030's freeze is a slapd that accepts connections and never answers; with
// collect-on-scrape and no bound, every scrape would park a goroutine and a
// connection on it. A watchdog releases the hang well after the timeout so a
// collector without one fails on elapsed time instead of hanging the suite.
//
// The bound is a socket deadline (ADR-033 revision after review): the
// collector hands the DialFunc the scrape's deadline, and the fake — like a
// real socket — fails the hung call when it passes. So this test pins the
// deadline the collector passes, and then the consequences of honouring it.
// A collector that bounded the scrape some other way (a timer abandoning a
// goroutine) but passed no deadline fails the first assertion.
func TestCollectorTimeoutBoundsAHungSlapd(t *testing.T) {
	const timeout = 200 * time.Millisecond
	const watchdog = 3 * time.Second
	for _, tc := range []struct {
		hang string
		want map[string]float64 // dial/bind counters after the one scrape
	}{
		{hangDial, map[string]float64{result(famDials, "fail"): 1}},
		{hangBind, map[string]float64{result(famDials, "ok"): 1, result(famBinds, "fail"): 1}},
		{hangSearch, map[string]float64{result(famDials, "ok"): 1, result(famBinds, "ok"): 1}},
	} {
		t.Run(tc.hang+" hangs", func(t *testing.T) {
			f := healthyFake(t)
			f.hang, f.release = tc.hang, make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(f.release) }) }
			defer release()
			dog := time.AfterFunc(watchdog, release)
			defer dog.Stop()

			opts := externalOpts()
			opts.Timeout = timeout
			reg := newScrapeTarget(t, opts, f)

			start := time.Now()
			got := scrape(t, reg)
			if elapsed := time.Since(start); elapsed > timeout+time.Second {
				t.Errorf("scrape took %v with a %v timeout; the hung %s was not bounded", elapsed, timeout, tc.hang)
			}
			if d, _, _, _ := f.counts(); d != 1 {
				t.Fatalf("%d dials; the scrape never reached slapd", d)
			}
			if dl := f.deadlines[0]; dl.IsZero() || dl.Before(start) || dl.After(start.Add(timeout+100*time.Millisecond)) {
				t.Errorf("dial deadline = %v, want start+%v (start %v); the scrape's bound is the socket deadline",
					dl, timeout, start)
			}
			if m := monitorKeys(t, got); len(m) != 0 {
				t.Errorf("hung %s still exported %d cn=Monitor series", tc.hang, len(m))
			}
			counters := byKey(t, filterSamples(samplesOf(got), func(s Sample) bool { return s.Name == famDials || s.Name == famBinds }))
			if !maps.Equal(counters, tc.want) {
				t.Errorf("attempt counters = %v, want %v", counters, tc.want)
			}
			if up, ok := byKey(t, samplesOf(got))[bare(famUp)]; !ok || up != 0 {
				t.Errorf("%s = %v (present=%v) after a hung %s, want 0", famUp, up, ok, tc.hang)
			}

			if tc.hang != hangDial {
				// The hung call is on a connection the collector holds: closing it
				// is what aborts the call (go-ldap's Close does), and it must
				// already have happened — not wait for slapd to wake up.
				if n := f.openConns(); n != 0 {
					t.Errorf("%d connection(s) still open after the scrape gave up on a hung %s", n, tc.hang)
				}
			}
			// Now slapd wakes up. A dial that completes late hands back a
			// connection nobody will use: it must be closed, not leaked.
			release()
			deadline := time.Now().Add(2 * time.Second)
			for f.openConns() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := f.openConns(); n != 0 {
				t.Errorf("%d connection(s) leaked after the hung %s returned", n, tc.hang)
			}
		})
	}
}
