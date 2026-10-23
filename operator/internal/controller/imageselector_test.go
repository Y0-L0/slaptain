package controller

import (
	"os"
	"strings"
	"testing"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The selector (ADR-040 amendment) names two axes — what can open your volumes,
// and whether you run our patches — and resolves to a tag SUFFIX on the
// operator's own tag. These assertions are the ADR's table, written before the
// resolution existed.
//
// The default cell carries NO suffix. That is ADR-040 decision 2 (the
// unsuffixed build is the pure one) and it is what keeps the charts'
// `image.tag | default .Chart.AppVersion` fallback an identity rather than a
// coincidence.
func TestImageTagSuffixForSelector(t *testing.T) {
	cases := []struct {
		name    string
		sel     *ldapv1alpha1.SlapdImageSelector
		want    string
		wantErr bool
	}{
		{
			name: "absent selector takes the defaults, which carry no suffix",
			sel:  nil,
			want: "",
		},
		{
			name: "empty selector is the same as an absent one",
			sel:  &ldapv1alpha1.SlapdImageSelector{},
			want: "",
		},
		{
			name: "the default cell, stated explicitly",
			sel:  &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb1.0", Flavour: "pure"},
			want: "",
		},
		{
			name: "current format, our patches",
			sel:  &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb1.0", Flavour: "slaptain"},
			want: "-ol2.7.1-slaptain",
		},
		{
			name: "legacy format, pure — Debian's own 2.6 binary",
			sel:  &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb0.9", Flavour: "pure"},
			want: "-ol26",
		},
		{
			name: "legacy format, our patches",
			sel:  &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb0.9", Flavour: "slaptain"},
			want: "-ol26src",
		},
		{
			name: "one axis set, the other defaulted",
			sel:  &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb0.9"},
			want: "-ol26",
		},
		{
			name: "the other axis set alone",
			sel:  &ldapv1alpha1.SlapdImageSelector{Flavour: "slaptain"},
			want: "-ol2.7.1-slaptain",
		},
		{
			name:    "an unknown storage format is refused, never guessed",
			sel:     &ldapv1alpha1.SlapdImageSelector{StorageFormat: "lmdb2.0"},
			wantErr: true,
		},
		{
			name:    "an unknown flavour is refused, never guessed",
			sel:     &ldapv1alpha1.SlapdImageSelector{Flavour: "patched"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := imageTagSuffixForSelector(tc.sel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("imageTagSuffixForSelector(%+v) = %q, want an error", tc.sel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("imageTagSuffixForSelector(%+v) returned %v", tc.sel, err)
			}
			if got != tc.want {
				t.Errorf("imageTagSuffixForSelector(%+v) = %q, want %q", tc.sel, got, tc.want)
			}
		})
	}
}

// The selector drives BOTH data-plane images from one declaration, which is how
// ADR-021's "pin both or neither" stops being a rule a user can violate. An
// explicit tag still wins: the selector picks a build, it does not take the
// pin away from someone who wants one.
func TestResolveImageRefWithSelector(t *testing.T) {
	const slapdRepo = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd"
	const initRepo = "ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-init"
	cases := []struct {
		name   string
		img    ldapv1alpha1.SlapdImageConfig
		repo   string
		suffix string
		want   string
	}{
		{
			name: "no selector, nothing pinned — byte-identical to the pre-selector default",
			img:  ldapv1alpha1.SlapdImageConfig{},
			repo: slapdRepo, suffix: "",
			want: slapdRepo + ":v0.0.1",
		},
		{
			name: "selector reaches the slapd image",
			img:  ldapv1alpha1.SlapdImageConfig{},
			repo: slapdRepo, suffix: "-ol26",
			want: slapdRepo + ":v0.0.1-ol26",
		},
		{
			name: "and the SAME selector reaches the init image",
			img:  ldapv1alpha1.SlapdImageConfig{},
			repo: initRepo, suffix: "-ol26",
			want: initRepo + ":v0.0.1-ol26",
		},
		{
			name: "an explicit tag wins over the selector, and is used verbatim",
			img:  ldapv1alpha1.SlapdImageConfig{Tag: "v9.9.9"},
			repo: slapdRepo, suffix: "-ol26",
			want: slapdRepo + ":v9.9.9",
		},
		{
			name: "an explicit repository still takes the selector's tag",
			img:  ldapv1alpha1.SlapdImageConfig{Repository: "example.com/custom/slapd"},
			repo: slapdRepo, suffix: "-ol26src",
			want: "example.com/custom/slapd:v0.0.1-ol26src",
		},
		{
			name: "the latest fallback carries the suffix too",
			img:  ldapv1alpha1.SlapdImageConfig{},
			repo: slapdRepo, suffix: "-ol26",
			want: slapdRepo + ":latest-ol26",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defaultTag := "v0.0.1"
			if tc.want == slapdRepo+":latest-ol26" {
				defaultTag = ""
			}
			got := resolveImageRefSelected(tc.img, tc.repo, defaultTag, tc.suffix)
			if got != tc.want {
				t.Errorf("resolveImageRefSelected(%+v, %q, %q, %q) = %q, want %q",
					tc.img, tc.repo, defaultTag, tc.suffix, got, tc.want)
			}
		})
	}
}

// The suffix table and the Makefile must agree about what the patched 2.7 pair
// is CALLED, and they derive it independently: the Makefile reads the OpenLDAP
// version out of debian/changelog, the table above spells it out. Two sources
// of truth that nothing compares will drift the day the changelog moves to
// 2.7.2 — and the symptom would be an ImagePullBackOff for everyone who
// selected flavour: slaptain, with both halves looking correct in review.
//
// Reading a repository file from a unit test is already how the CRD rule tests
// guard controller-gen's output; same reasoning, same shape.
func TestSuffixTableAgreesWithTheBuiltOpenLDAPVersion(t *testing.T) {
	const changelog = "../../../images/openldap-deb/debian/changelog"
	raw, err := os.ReadFile(changelog)
	if err != nil {
		t.Fatalf("read %s: %v", changelog, err)
	}
	// Top entry: "openldap (2.7.1-0+slaptain3) unstable; urgency=medium"
	first, _, _ := strings.Cut(string(raw), "\n")
	open, close := strings.Index(first, "("), strings.Index(first, ")")
	if open < 0 || close < open {
		t.Fatalf("cannot read a version out of %q", first)
	}
	version, _, _ := strings.Cut(first[open+1:close], "-")
	if version == "" {
		t.Fatalf("empty version in %q", first)
	}
	want := "-ol" + version + "-slaptain"
	if got := imageTagSuffixes[storageFormatCurrent][flavourSlaptain]; got != want {
		t.Errorf("the selector resolves flavour %q to %q, but the Makefile builds that pair as %q "+
			"(derived from %s). Update imageTagSuffixes.", flavourSlaptain, got, want, changelog)
	}
}
