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
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ─── The exposition, as a contract ──────────────────────────────────────────
//
// Types are a property of the exposition, not of the collector: what a
// Prometheus server stores is the "# TYPE" line it parsed. So these tests run
// through the HTTP handler under each Accept header a server may send.

// Accept headers. prometheusAccept is what a Prometheus 3 server sends by
// default (scrape_protocols: OpenMetricsText1.0.0, OpenMetricsText0.0.1,
// PrometheusText1.0.0, PrometheusText0.0.4) — OpenMetrics preferred.
const (
	acceptText        = "text/plain;version=0.0.4"
	acceptOpenMetrics = "application/openmetrics-text;version=1.0.0"
	prometheusAccept  = "application/openmetrics-text;version=1.0.0;escaping=allow-utf-8;q=0.6," +
		"application/openmetrics-text;version=0.0.1;q=0.5," +
		"text/plain;version=1.0.0;escaping=allow-utf-8;q=0.4," +
		"text/plain;version=0.0.4;q=0.3,*/*;q=0.2"
)

// expoSample is one sample line of an exposition, format-agnostic: the series
// name as a Prometheus server would store it, plus its labels and value.
type expoSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition reads the sample lines of a text or OpenMetrics exposition.
// Deliberately format-agnostic, so whichever the handler negotiates, the
// assertion is about the names that end up in Prometheus.
func parseExposition(t *testing.T, body string) []expoSample {
	t.Helper()
	out := make([]expoSample, 0, strings.Count(body, "\n"))
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := expoSample{labels: map[string]string{}}
		rest := line
		if i := strings.IndexAny(rest, "{ "); i >= 0 {
			s.name, rest = rest[:i], rest[i:]
		}
		if strings.HasPrefix(rest, "{") {
			rest = rest[1:]
			for !strings.HasPrefix(rest, "}") {
				eq := strings.Index(rest, "=")
				if eq < 0 || len(rest) < eq+2 || rest[eq+1] != '"' {
					t.Fatalf("bad label set in %q", line)
				}
				name := strings.TrimPrefix(rest[:eq], ",")
				val, n, err := unquotePrefix(rest[eq+1:])
				if err != nil {
					t.Fatalf("bad label value in %q: %v", line, err)
				}
				s.labels[name] = val
				rest = strings.TrimPrefix(rest[eq+1+n:], ",")
			}
			rest = rest[1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			t.Fatalf("no value in %q", line)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("bad value in %q: %v", line, err)
		}
		s.value = v
		out = append(out, s)
	}
	return out
}

// unquotePrefix reads one Go/Prometheus-quoted string at the start of s and
// returns it with the number of bytes consumed.
func unquotePrefix(s string) (string, int, error) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			v, err := strconv.Unquote(s[:i+1])
			return v, i + 1, err
		}
	}
	return "", 0, io.ErrUnexpectedEOF
}

// get scrapes path with the given Accept header and returns the status, the
// negotiated Content-Type and the body.
func get(t *testing.T, h http.Handler, path, accept string) (code int, contentType, body string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw)
}

// typeLines maps each "# TYPE <name> <type>" of an exposition.
func typeLines(body string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		if f := strings.Fields(line); len(f) == 4 && f[0] == "#" && f[1] == "TYPE" {
			out[f[2]] = f[3]
		}
	}
	return out
}

// wantTypes is ADR-033 decision 4's type per family: counters are exactly the
// _total families.
func wantTypes() map[string]string {
	want := map[string]string{}
	for _, f := range []string{famUp, famDials, famBinds, famOpsInitiated, famOpsCompleted, famConnsTotal,
		famConnsCurrent, famMaxFDs, famSentBytes, famSentPDUs, famSentEntries, famSentReferrals,
		famThreadsOpen, famThreadsActive, famThreadsPending, famThreadsStart, famThreadsBack,
		famThreadsMax, famThreadsMaxPend, famWaiters, famStartTime} {
		want[f] = "gauge"
		if strings.HasSuffix(f, "_total") {
			want[f] = "counter"
		}
	}
	for _, f := range mdbFamilies {
		want[f] = "gauge"
	}
	return want
}

// TestHandlerTypesEveryFamilyUnderEveryNegotiation: a healthy scrape declares
// "# TYPE <family> counter" for every _total family and "gauge" for the rest,
// whichever format the handler negotiates, and every family has samples under
// its full name. OpenMetrics names a counter's TYPE line without the _total
// suffix its samples carry; text names both the same.
func TestHandlerTypesEveryFamilyUnderEveryNegotiation(t *testing.T) {
	h := NewHandler(NewCollector(externalOpts(), healthyFake(t).dial))
	for _, accept := range []string{acceptText, acceptOpenMetrics, prometheusAccept} {
		t.Run(accept, func(t *testing.T) {
			code, ctype, body := get(t, h, MetricsPath, accept)
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200\n%s", MetricsPath, code, body)
			}
			om := strings.HasPrefix(ctype, "application/openmetrics-text")
			t.Logf("Accept %q negotiated Content-Type %q", accept, ctype)
			types := typeLines(body)
			var names []string
			for _, s := range parseExposition(t, body) {
				names = append(names, s.name)
			}
			for fam, want := range wantTypes() {
				typeName := fam
				if om && want == "counter" {
					typeName = strings.TrimSuffix(fam, "_total")
				}
				if got := types[typeName]; got != want {
					t.Errorf("# TYPE %s = %q, want %q (Content-Type %s)", typeName, got, want, ctype)
				}
				if !slices.Contains(names, fam) {
					t.Errorf("no sample named %s (Content-Type %s)", fam, ctype)
				}
			}
		})
	}
}

// TestHandlerFailedReadIsUpZeroAndNoMonitorFamily: whichever step fails, the
// scrape is a 200 carrying openldap_up 0 and the exporter's own counters, and
// no cn=Monitor family at all — no sample and no TYPE line. Not a 500, which
// would turn every series absent including the one that says why, and not
// zeros (ADR-033 decision 5). Repeated scrapes keep counting the failure.
func TestHandlerFailedReadIsUpZeroAndNoMonitorFamily(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    func(*fakeSlapd)
		counter string // the exporter counter whose result="fail" counts the failure; "" for none
	}{
		{"dial fails", func(f *fakeSlapd) { f.dialErr = errFake }, famDials},
		{"bind fails", func(f *fakeSlapd) { f.bindErr = errFake }, famBinds},
		{"search fails", func(f *fakeSlapd) { f.searchErr = errFake }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake(t)
			f.set(tc.fail)
			h := NewHandler(NewCollector(externalOpts(), f.dial))
			for i := 1; i <= 2; i++ {
				code, ctype, body := get(t, h, MetricsPath, prometheusAccept)
				if code != http.StatusOK {
					t.Fatalf("scrape %d: GET %s = %d, want 200\n%s", i, MetricsPath, code, body)
				}
				var up, fails float64 = -1, -1
				for _, s := range parseExposition(t, body) {
					switch {
					case s.name == famUp:
						up = s.value
					case s.name == tc.counter && s.labels["result"] == "fail":
						fails = s.value
					case isMonitorFamily(s.name):
						t.Errorf("scrape %d: %s %v exported with no cn=Monitor read", i, s.name, s.labels)
					}
				}
				for name := range typeLines(body) {
					if isMonitorFamily(name) {
						t.Errorf("scrape %d: # TYPE %s declared with no cn=Monitor read (Content-Type %s)", i, name, ctype)
					}
				}
				if up != 0 {
					t.Errorf("scrape %d: %s = %v, want 0 (-1: absent)", i, famUp, up)
				}
				if tc.counter != "" && fails != float64(i) {
					t.Errorf(`scrape %d: %s{result="fail"} = %v, want %d`, i, tc.counter, fails, i)
				}
			}
		})
	}
}
