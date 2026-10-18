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

	"sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── ADR-037 (second amendment): the rotation block round-trips ───────────────

// The rotation block is three values the operator must be able to read back
// exactly as the user wrote them: whether rotation is on, the slot length t,
// and v, the signed displacement from a pod's write slot to its purge slot.
// Each of the three has a state that a careless shape would destroy — an unset
// `enabled` that must stay distinguishable from an explicit false (the LogLevel
// *int32 argument, applied to a bool), a duration that must survive
// metav1.Duration's string JSON form, and a slotsFromWriteToPurge whose unset
// state means "default to 1 in the controller" rather than 0, AND whose
// NEGATIVE values are legal: −1 is n−1 taken mod n, which is the whole reason
// the field is signed (ADR-037 ninth amendment).
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
			absentJSON: []string{`"enabled"`, `"slotLength"`, `"slotsFromWriteToPurge"`},
		},
		{
			name:       "enabled false is explicit and distinguishable from unset",
			in:         SlapdRotationConfig{Enabled: new(false)},
			wantJSON:   []string{`"enabled":false`},
			absentJSON: []string{`"slotLength"`, `"slotsFromWriteToPurge"`},
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
			name: "slotsFromWriteToPurge 1 survives, and is not swallowed as a zero value",
			in: SlapdRotationConfig{
				Enabled:               new(true),
				SlotLength:            new(metav1.Duration{Duration: 90 * time.Minute}),
				SlotsFromWriteToPurge: new(int32(1)),
			},
			wantJSON: []string{`"slotLength":"1h30m0s"`, `"slotsFromWriteToPurge":1`},
		},
		{
			// The sign is the feature. "−1" must reach the operator as −1, not
			// be refused by a minimum marker and not be rewritten to anything:
			// it means "purge the slot BEFORE writing", stays correct across a
			// scale event, and the operator resolves it mod spec.replicas.
			name: "a negative slotsFromWriteToPurge round-trips unchanged",
			in: SlapdRotationConfig{
				Enabled:               new(true),
				SlotLength:            new(metav1.Duration{Duration: 6 * time.Hour}),
				SlotsFromWriteToPurge: new(int32(-1)),
			},
			wantJSON: []string{`"slotsFromWriteToPurge":-1`},
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
			case tc.in.SlotsFromWriteToPurge == nil && back.Rotation.SlotsFromWriteToPurge != nil:
				t.Errorf("slotsFromWriteToPurge = %v, want nil", *back.Rotation.SlotsFromWriteToPurge)
			case tc.in.SlotsFromWriteToPurge != nil && back.Rotation.SlotsFromWriteToPurge == nil:
				t.Errorf("slotsFromWriteToPurge = nil, want %v", *tc.in.SlotsFromWriteToPurge)
			case tc.in.SlotsFromWriteToPurge != nil && *back.Rotation.SlotsFromWriteToPurge != *tc.in.SlotsFromWriteToPurge:
				t.Errorf("slotsFromWriteToPurge = %v, want %v", *back.Rotation.SlotsFromWriteToPurge, *tc.in.SlotsFromWriteToPurge)
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
		Enabled:               new(true),
		SlotLength:            new(metav1.Duration{Duration: 12 * time.Hour}),
		SlotsFromWriteToPurge: new(int32(2)),
	}

	cp := sc.DeepCopy()
	if cp.Spec.Rotation.Enabled == sc.Spec.Rotation.Enabled {
		t.Error("enabled pointer is shared between the copy and the original")
	}
	if cp.Spec.Rotation.SlotsFromWriteToPurge == sc.Spec.Rotation.SlotsFromWriteToPurge {
		t.Error("slotsFromWriteToPurge pointer is shared between the copy and the original")
	}
	*cp.Spec.Rotation.SlotsFromWriteToPurge = 1
	if *sc.Spec.Rotation.SlotsFromWriteToPurge != 2 {
		t.Errorf("mutating the copy changed the original: slotsFromWriteToPurge = %d, want 2",
			*sc.Spec.Rotation.SlotsFromWriteToPurge)
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

// ── ADR-024 R6: the decomposed purge fields ─────────────────────────────────

// durationRule is the regex the CRD's CEL rules apply to every metav1.Duration
// in this API. Kept as one copy, pinned against the generated CRD below.
const durationRule = slotLengthRule

// The grammar rules must be IN the generated CRD for all three durations, not
// only for slotLength. controller-gen renders metav1.Duration as a bare
// `type: string` and silently drops field-level validation markers on it, so a
// struct-level CEL rule is the only shape that survives — and a rule that was
// written as a field marker by mistake would leave the field unvalidated while
// every unit test still passed.
func TestEveryDurationFieldCarriesItsGrammarRuleInTheCRD(t *testing.T) {
	cases := []struct {
		crd   string
		field string
	}{
		{"../../config/crd/bases/ldap.chuck-chuck-chuck.net_slapdclusters.yaml", "self.slotLength"},
		{"../../config/crd/bases/ldap.chuck-chuck-chuck.net_slapdclusters.yaml", "self.interval"},
		{"../../config/crd/bases/ldap.chuck-chuck-chuck.net_slapddatabases.yaml", "self.retention"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			raw, err := os.ReadFile(c.crd)
			if err != nil {
				t.Fatalf("read %s: %v", c.crd, err)
			}
			// The generator emits the CEL rule single-quoted, doubling inner quotes.
			want := c.field + ".matches(''" + durationRule + "'')"
			if !strings.Contains(string(raw), want) {
				t.Errorf("generated CRD %s does not carry the %s CEL rule", c.crd, c.field)
			}
		})
	}
}

// ADR-037's ninth amendment retires `minimum: 1` on the writer displacement:
// the rule is `v mod n != 0`, which needs spec.replicas and is therefore the
// OPERATOR's to report, not the API server's to refuse. A surviving minimum
// would refuse −1 — the value the sign convention exists for — at admission,
// and the operator's condition would never get the chance to resolve it.
func TestSlotsFromWriteToPurgeHasNoMinimum(t *testing.T) {
	const crd = "../../config/crd/bases/ldap.chuck-chuck-chuck.net_slapdclusters.yaml"
	raw, err := os.ReadFile(crd)
	if err != nil {
		t.Fatalf("read %s: %v", crd, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", crd, err)
	}
	spec := doc["spec"].(map[string]any)
	versions := spec["versions"].([]any)
	schema := versions[0].(map[string]any)["schema"].(map[string]any)
	props := schema["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)
	clusterSpec := props["spec"].(map[string]any)["properties"].(map[string]any)
	rotation := clusterSpec["rotation"].(map[string]any)["properties"].(map[string]any)
	field, ok := rotation["slotsFromWriteToPurge"].(map[string]any)
	if !ok {
		t.Fatal("rotation.slotsFromWriteToPurge is missing from the generated CRD")
	}
	if _, has := field["minimum"]; has {
		t.Errorf("rotation.slotsFromWriteToPurge still carries a minimum (%v); "+
			"it would refuse −1, which is the value the signed convention exists for",
			field["minimum"])
	}
	if got := field["type"]; got != "integer" {
		t.Errorf("rotation.slotsFromWriteToPurge type = %v, want integer", got)
	}
}

// The decomposed purge fields have exactly the states a careless shape
// destroys, and each one of them is load-bearing (ADR-024 R6):
//
//   - `enabled: false` means "never purge, keep forever". A plain bool with
//     omitempty would serialise it to nothing and make it indistinguishable
//     from silence, which defaults to purging ON — so the user who asked for
//     no purge would get one.
//   - an unset retention or interval must NOT serialise as "0s". metav1.Duration
//     is a struct, so omitempty does not apply to a value field and the
//     operator would read a retention nobody chose — zero, which composePurge
//     reads as "no purge value at all".
func TestDecomposedPurgeFieldsRoundTrip(t *testing.T) {
	t.Run("an unset database block claims nothing", func(t *testing.T) {
		var sd SlapdDatabaseSpec
		raw, err := json.Marshal(sd)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "accesslogPurge") {
			t.Errorf("an unset accesslogPurge serialised anyway: %s", raw)
		}
	})

	t.Run("enabled false survives and retention stays unset", func(t *testing.T) {
		var sd SlapdDatabaseSpec
		sd.Replication.AccesslogPurge = &AccesslogPurgeConfig{Enabled: new(false)}
		raw, err := json.Marshal(sd)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(raw)
		if !strings.Contains(got, `"enabled":false`) {
			t.Errorf("explicit enabled:false did not survive: %s", got)
		}
		if strings.Contains(got, `"retention"`) {
			t.Errorf("unset retention serialised anyway (a \"0s\" claims a value nobody "+
				"chose): %s", got)
		}
		var back SlapdDatabaseSpec
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if back.Replication.AccesslogPurge == nil || back.Replication.AccesslogPurge.Enabled == nil ||
			*back.Replication.AccesslogPurge.Enabled {
			t.Errorf("enabled:false did not round-trip: %+v", back.Replication.AccesslogPurge)
		}
		if back.Replication.AccesslogPurge.Retention != nil {
			t.Errorf("retention = %v, want nil", *back.Replication.AccesslogPurge.Retention)
		}
	})

	t.Run("a declared retention round-trips through metav1.Duration", func(t *testing.T) {
		var sd SlapdDatabaseSpec
		sd.Replication.AccesslogPurge = &AccesslogPurgeConfig{
			Retention: new(metav1.Duration{Duration: 48 * time.Hour}),
		}
		raw, err := json.Marshal(sd)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"retention":"48h0m0s"`) {
			t.Errorf("retention did not serialise as a duration string: %s", raw)
		}
	})

	t.Run("the cluster's form-A interval round-trips, and unset stays unset", func(t *testing.T) {
		var sc SlapdClusterSpec
		raw, err := json.Marshal(sc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "accesslogPurge") {
			t.Errorf("an unset spec.accesslogPurge serialised anyway: %s", raw)
		}

		sc.AccesslogPurge = &SlapdAccesslogPurgeConfig{
			Interval: new(metav1.Duration{Duration: 24 * time.Hour}),
		}
		raw, err = json.Marshal(sc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"accesslogPurge":{"interval":"24h0m0s"}`) {
			t.Errorf("form-A interval did not round-trip: %s", raw)
		}
	})
}

// DeepCopy on the decomposed blocks: a shallow pointer copy would let a
// controller editing a deep copy corrupt its own cache entry.
func TestDecomposedPurgeDeepCopyDoesNotAlias(t *testing.T) {
	sd := &SlapdDatabase{}
	sd.Spec.Replication.AccesslogPurge = &AccesslogPurgeConfig{
		Enabled:   new(true),
		Retention: new(metav1.Duration{Duration: 168 * time.Hour}),
	}
	cp := sd.DeepCopy()
	if cp.Spec.Replication.AccesslogPurge == sd.Spec.Replication.AccesslogPurge {
		t.Error("accesslogPurge pointer is shared between the copy and the original")
	}
	if cp.Spec.Replication.AccesslogPurge.Retention == sd.Spec.Replication.AccesslogPurge.Retention {
		t.Error("retention pointer is shared between the copy and the original")
	}
	*cp.Spec.Replication.AccesslogPurge.Retention = metav1.Duration{Duration: time.Hour}
	if sd.Spec.Replication.AccesslogPurge.Retention.Duration != 168*time.Hour {
		t.Errorf("mutating the copy changed the original: retention = %v",
			sd.Spec.Replication.AccesslogPurge.Retention.Duration)
	}

	sc := &SlapdCluster{}
	sc.Spec.AccesslogPurge = &SlapdAccesslogPurgeConfig{
		Interval: new(metav1.Duration{Duration: 24 * time.Hour}),
	}
	ccp := sc.DeepCopy()
	if ccp.Spec.AccesslogPurge == sc.Spec.AccesslogPurge {
		t.Error("spec.accesslogPurge pointer is shared between the copy and the original")
	}
	*ccp.Spec.AccesslogPurge.Interval = metav1.Duration{Duration: time.Hour}
	if sc.Spec.AccesslogPurge.Interval.Duration != 24*time.Hour {
		t.Errorf("mutating the copy changed the original: interval = %v",
			sc.Spec.AccesslogPurge.Interval.Duration)
	}
}
