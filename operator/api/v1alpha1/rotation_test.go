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

package v1alpha1

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── ADR-037 (second amendment): the rotation block round-trips ───────────────

// The rotation block is three values the operator must be able to read back
// exactly as the user wrote them: whether rotation is on, the slot length t,
// and the writer offset d. Each of the three has a state that a careless shape
// would destroy — an unset `enabled` that must stay distinguishable from an
// explicit false (the LogLevel *int32 argument, applied to a bool), a duration
// that must survive metav1.Duration's string JSON form, and a writerOffset
// whose unset state means "default to n−1 in the controller" rather than 0.
func TestRotationConfigRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   SlapdRotationConfig
		// wantJSON fragments that must appear in the serialised spec.
		wantJSON []string
		// absentJSON fragments that must NOT appear (omitempty behaviour).
		absentJSON []string
	}{
		{
			name: "an unset rotation block claims nothing",
			in:   SlapdRotationConfig{},
			// The block is embedded by value and renders as an empty object,
			// as every other spec family does; what must not appear is a VALUE
			// the user never wrote — notably a slotLength of "0s", which
			// metav1.Duration's non-pointer form emits unconditionally.
			absentJSON: []string{`"enabled"`, `"slotLength"`, `"writerOffset"`},
		},
		{
			name:       "enabled false is explicit and distinguishable from unset",
			in:         SlapdRotationConfig{Enabled: new(false)},
			wantJSON:   []string{`"enabled":false`},
			absentJSON: []string{`"slotLength"`, `"writerOffset"`},
		},
		{
			name: "a six-hour slot length round-trips through metav1.Duration",
			in: SlapdRotationConfig{
				Enabled:    new(true),
				SlotLength: new(metav1.Duration{Duration: 6 * time.Hour}),
			},
			wantJSON: []string{`"enabled":true`, `"slotLength":"6h0m0s"`},
		},
		{
			name: "writerOffset 1 survives, and is not swallowed as a zero value",
			in: SlapdRotationConfig{
				Enabled:      new(true),
				SlotLength:   new(metav1.Duration{Duration: 90 * time.Minute}),
				WriterOffset: new(int32(1)),
			},
			wantJSON: []string{`"slotLength":"1h30m0s"`, `"writerOffset":1`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &SlapdCluster{}
			sc.Spec.Rotation = tc.in

			raw, err := json.Marshal(sc.Spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(raw)
			for _, want := range tc.wantJSON {
				if !strings.Contains(got, want) {
					t.Errorf("serialised spec is missing %s\n got: %s", want, got)
				}
			}
			for _, absent := range tc.absentJSON {
				if strings.Contains(got, absent) {
					t.Errorf("serialised spec must not contain %s\n got: %s", absent, got)
				}
			}

			var back SlapdClusterSpec
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			switch {
			case tc.in.SlotLength == nil && back.Rotation.SlotLength != nil:
				t.Errorf("slotLength = %v, want nil (unset must stay unset, not 0s)", *back.Rotation.SlotLength)
			case tc.in.SlotLength != nil && back.Rotation.SlotLength == nil:
				t.Errorf("slotLength = nil, want %v", *tc.in.SlotLength)
			case tc.in.SlotLength != nil && *back.Rotation.SlotLength != *tc.in.SlotLength:
				t.Errorf("slotLength = %v, want %v", *back.Rotation.SlotLength, *tc.in.SlotLength)
			}
			switch {
			case tc.in.Enabled == nil && back.Rotation.Enabled != nil:
				t.Errorf("enabled = %v, want nil (unset must stay unset)", *back.Rotation.Enabled)
			case tc.in.Enabled != nil && back.Rotation.Enabled == nil:
				t.Errorf("enabled = nil, want %v (an explicit value must survive)", *tc.in.Enabled)
			case tc.in.Enabled != nil && *back.Rotation.Enabled != *tc.in.Enabled:
				t.Errorf("enabled = %v, want %v", *back.Rotation.Enabled, *tc.in.Enabled)
			}
			switch {
			case tc.in.WriterOffset == nil && back.Rotation.WriterOffset != nil:
				t.Errorf("writerOffset = %v, want nil", *back.Rotation.WriterOffset)
			case tc.in.WriterOffset != nil && back.Rotation.WriterOffset == nil:
				t.Errorf("writerOffset = nil, want %v", *tc.in.WriterOffset)
			case tc.in.WriterOffset != nil && *back.Rotation.WriterOffset != *tc.in.WriterOffset:
				t.Errorf("writerOffset = %v, want %v", *back.Rotation.WriterOffset, *tc.in.WriterOffset)
			}
		})
	}
}

// DeepCopy is generated, and a pointer field copied shallowly would alias the
// original — editing a deep copy would then edit the object it was copied from,
// which is how a controller corrupts its own cache entry.
func TestRotationConfigDeepCopyDoesNotAlias(t *testing.T) {
	sc := &SlapdCluster{}
	sc.Spec.Rotation = SlapdRotationConfig{
		Enabled:      new(true),
		SlotLength:   new(metav1.Duration{Duration: 12 * time.Hour}),
		WriterOffset: new(int32(2)),
	}

	cp := sc.DeepCopy()
	if cp.Spec.Rotation.Enabled == sc.Spec.Rotation.Enabled {
		t.Error("enabled pointer is shared between the copy and the original")
	}
	if cp.Spec.Rotation.WriterOffset == sc.Spec.Rotation.WriterOffset {
		t.Error("writerOffset pointer is shared between the copy and the original")
	}
	*cp.Spec.Rotation.WriterOffset = 1
	if *sc.Spec.Rotation.WriterOffset != 2 {
		t.Errorf("mutating the copy changed the original: writerOffset = %d, want 2",
			*sc.Spec.Rotation.WriterOffset)
	}
	if cp.Spec.Rotation.SlotLength == sc.Spec.Rotation.SlotLength {
		t.Error("slotLength pointer is shared between the copy and the original")
	}
	if *cp.Spec.Rotation.SlotLength != *sc.Spec.Rotation.SlotLength {
		t.Errorf("slotLength did not survive DeepCopy: %v, want %v",
			*cp.Spec.Rotation.SlotLength, *sc.Spec.Rotation.SlotLength)
	}
}

// slotLengthRule is the regex the CRD's CEL rule applies to rotation.slotLength.
// It is kept here as a copy on purpose: the test below pins it against the
// GENERATED CRD, so editing the marker without re-running the generator — or
// re-running the generator without thinking about what the rule now accepts —
// goes red instead of silently diverging.
const slotLengthRule = `^([0-9]+([.][0-9]+)?(ns|us|µs|ms|s|m|h))+$`

// ADR-037: slotLength carries t, which the controller will parse with Go's
// duration parser. The admission rule and that parser must agree, or the API
// admits values the operator cannot read — or refuses ones it could.
// CEL's matches() is RE2, which is also Go's regexp engine, so agreement here
// is agreement there.
func TestSlotLengthRuleAgreesWithGoDurationParsing(t *testing.T) {
	re := regexp.MustCompile(slotLengthRule)
	cases := []struct {
		in         string
		wantAccept bool
	}{
		{"6h", true},
		{"12h", true},
		{"90m", true},
		{"12h30m", true},
		{"500ms", true},
		{"1.5h", true},
		{"banana", false},
		{"6 h", false},
		{"6H", false},
		{"1d", false},  // Go has no day unit; a user writing it must be told now
		{"-5m", false}, // a negative slot length is not a schedule
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := re.MatchString(c.in); got != c.wantAccept {
				t.Fatalf("rule accepts %q = %v, want %v", c.in, got, c.wantAccept)
			}
			if !c.wantAccept {
				return
			}
			if _, err := time.ParseDuration(c.in); err != nil {
				t.Fatalf("rule accepts %q but time.ParseDuration rejects it: %v", c.in, err)
			}
		})
	}
}

// The rule must actually be in the generated CRD; a marker that controller-gen
// silently dropped — which is exactly what a Pattern marker on a
// metav1.Duration field does — would leave the field unvalidated while the
// test above still passed.
func TestSlotLengthRuleIsInTheGeneratedCRD(t *testing.T) {
	const crd = "../../config/crd/bases/ldap.chuck-chuck-chuck.net_slapdclusters.yaml"
	raw, err := os.ReadFile(crd)
	if err != nil {
		t.Fatalf("read %s: %v", crd, err)
	}
	// The generator emits the CEL rule single-quoted, doubling inner quotes.
	want := "self.slotLength.matches(''" + slotLengthRule + "'')"
	if !strings.Contains(string(raw), want) {
		t.Errorf("generated CRD does not carry the slotLength CEL rule %s", slotLengthRule)
	}
}
