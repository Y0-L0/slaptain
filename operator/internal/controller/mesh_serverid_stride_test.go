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
	"testing"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// TestLayoutPodsPerSiteAgainstServerIDStride pins the bound that the hex
// serverID stride creates and the decimal one hid.
//
// A pod's serverID is serverIDBase + ordinal + 1 (ADR-017) and a site's base is
// serverIDIndex * serverIDStride. With a stride of 16, site 0's pod ordinal 15
// takes sid 16 — the base of site 1, whose own pod ordinal 0 takes 17. The
// overlap is one slot wide and entirely silent: slapd validates nothing, and
// two pods writing under one sid read each other's writes as already-seen.
// Fifteen pods per site is therefore the ceiling.
//
// ridLayout.podsPerSite is where that bound belongs, because the operator
// already refuses spec.replicas > podsPerSite (ridstanza.go, reason
// PodsPerSiteExceeded). Capping the DECLARED layout keeps podsPerSite the
// single bound on replicas, instead of adding a second, separately-reasoned
// replicas check that could disagree with the first.
func TestLayoutPodsPerSiteAgainstServerIDStride(t *testing.T) {
	tests := []struct {
		name    string
		layout  ridcoord.Layout
		wantErr error
	}{
		{"default layout is well inside the stride", ridcoord.DefaultLayout(), nil},
		{"the last legal value", ridcoord.Layout{PodsPerSite: 15, MaxSites: 12}, nil},
		{"one past the stride", ridcoord.Layout{PodsPerSite: 16, MaxSites: 12}, errPodsPerSitePastStride},
		{"far past the stride", ridcoord.Layout{PodsPerSite: 64, MaxSites: 4}, errPodsPerSitePastStride},
		// maxSites is NOT bounded by the stride in the same way: site indices
		// run to maxSiteIndex (4094/stride), far above any layout's own
		// maxSites ceiling of 999, and validateSiteIndicesAgainstLayout
		// already holds the indices themselves to both ceilings.
		{"a wide site axis is not this check's business", ridcoord.Layout{PodsPerSite: 8, MaxSites: 999}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLayoutAgainstServerIDStride(tc.layout)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateLayoutAgainstServerIDStride(%+v) = %v, want nil", tc.layout, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateLayoutAgainstServerIDStride(%+v) = %v, want %v", tc.layout, err, tc.wantErr)
			}
		})
	}
}
