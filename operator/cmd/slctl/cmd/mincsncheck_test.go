package cmd

import (
	"strings"
	"testing"
)

// csn builds a CSN with the given time prefix and three-hex-digit sid.
func csn(ts, sid string) string { return ts + ".000000Z#000000#" + sid + "#000000" }

func TestCheckAccesslogMinCSN(t *testing.T) {
	const db, db2 = "cn=accesslog-example-db", "cn=accesslog-example-db2"
	clean := func(pod, log string) logCSNReading {
		return logCSNReading{Pod: pod, Log: log,
			MinCSN:     []string{csn("20261011135702", "001"), csn("20261011140738", "002")},
			ContextCSN: []string{csn("20261011141903", "001"), csn("20261011141901", "002"), csn("20261011141901", "003")},
		}
	}

	tests := []struct {
		name       string
		readings   []logCSNReading
		wantStatus string
		// every string must appear in Detail
		wantDetail []string
		// no string may appear in Detail
		notDetail []string
	}{
		{
			name:       "no accesslog databases at all",
			readings:   nil,
			wantStatus: checkPass,
			wantDetail: []string{"no accesslog"},
		},
		{
			name:       "every minCSN sid tracked; contextCSN tracking MORE is the benign direction",
			readings:   []logCSNReading{clean(testPod0, db), clean(testPod1, db)},
			wantStatus: checkPass,
		},
		{
			name: "no minCSN yet: nothing to contradict",
			readings: []logCSNReading{{Pod: testPod0, Log: db,
				ContextCSN: []string{csn("20261011141903", "001")}}},
			wantStatus: checkPass,
		},
		{
			// Captured live on site-1 slapd-0, 2026-10-11, while it refused
			// every consumer of this log with "sync cookie is stale".
			name: "live capture: minCSN sid 012 absent from contextCSN",
			readings: []logCSNReading{clean(testPod1, db), {Pod: testPod0, Log: db,
				MinCSN: []string{csn("20261011135702", "001"), csn("20261011140141", "003"),
					csn("20261011140149", "012"), csn("20261011140738", "002"), csn("20261011141819", "013")},
				ContextCSN: []string{csn("20261011141819", "013"), csn("20261011141901", "002"),
					csn("20261011141901", "003"), csn("20261011141903", "001")},
			}},
			wantStatus: checkFail,
			wantDetail: []string{testPod0, db, "012", "ITS#9398"},
			notDetail:  []string{testPod1, "013"},
		},
		{
			name: "live capture: two sids missing on the second database",
			readings: []logCSNReading{{Pod: testPod0, Log: db2,
				MinCSN:     []string{csn("20261011135651", "001"), csn("20261011140149", "003"), csn("20261011140152", "013")},
				ContextCSN: []string{csn("20261011141342", "001")},
			}},
			wantStatus: checkFail,
			wantDetail: []string{testPod0, db2, "003", "013"},
		},
		{
			name: "minCSN present, contextCSN empty: every sid is missing",
			readings: []logCSNReading{{Pod: testPod2, Log: db,
				MinCSN: []string{csn("20261011135702", "001")}}},
			wantStatus: checkFail,
			wantDetail: []string{testPod2, "001"},
		},
		{
			name: "sids compare case-insensitively (hex)",
			readings: []logCSNReading{{Pod: testPod0, Log: db,
				MinCSN:     []string{csn("20261011135702", "01A")},
				ContextCSN: []string{csn("20261011141903", "01a")},
			}},
			wantStatus: checkPass,
		},
		{
			name:       "an unreadable log never counts toward pass",
			readings:   []logCSNReading{clean(testPod0, db), {Pod: testPod1, Log: db, Err: "cn=config bind failed"}},
			wantStatus: "warn",
			wantDetail: []string{testPod1, db, "not read"},
		},
		{
			name: "a storm outranks an unreadable log, and both are named",
			readings: []logCSNReading{
				{Pod: testPod1, Log: db, Err: "timeout"},
				{Pod: testPod0, Log: db, MinCSN: []string{csn("20261011140149", "012")}},
			},
			wantStatus: checkFail,
			wantDetail: []string{testPod0, "012", testPod1, "not read"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkAccesslogMinCSN(tt.readings)
			if got.Name != "accesslog-mincsn" {
				t.Errorf("Name = %q, want accesslog-mincsn", got.Name)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", got.Status, tt.wantStatus, got.Detail)
			}
			for _, s := range tt.wantDetail {
				if !strings.Contains(got.Detail, s) {
					t.Errorf("Detail %q lacks %q", got.Detail, s)
				}
			}
			for _, s := range tt.notDetail {
				if strings.Contains(got.Detail, s) {
					t.Errorf("Detail %q should not mention %q", got.Detail, s)
				}
			}
		})
	}
}
