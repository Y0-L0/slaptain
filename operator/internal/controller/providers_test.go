/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-031 keys a syncrepl RID on the provider's IDENTITY, and the only place a
// remote pod's identity can be read from is its StatefulSet name. These tests
// pin the parse and the reduction that together turn "a list of IPs" into "a
// list of providers".
//
// RID-PLAN hazard 6 is the whole point: position in a discovery list carries
// no identity, so the ordinal is parsed and a name that will not parse is a
// refusal naming the pod — never a positional fallback.

func TestPodOrdinal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		podName string
		want    int32
		wantErr bool
		because string
	}{
		{
			name:    "the text after the last dash is the ordinal",
			podName: "slapd-3",
			want:    3,
			because: "a StatefulSet names its pods <set>-<ordinal>",
		},
		{
			name:    "a prefix containing dashes keeps the LAST dash as the separator",
			podName: "my-cluster-slapd-11",
			want:    11,
			because: "cluster names contain dashes; only the final segment is the ordinal",
		},
		{
			name:    "ordinal zero parses, and is not confused with absence",
			podName: "slapd-0",
			want:    0,
			because: "pod-0 is an ordinary provider and 0 is its real ordinal",
		},
		{
			name:    "a multi-digit ordinal parses whole",
			podName: "slapd-123",
			want:    123,
			because: "truncating to the last digit would alias 123 onto 3",
		},
		{
			// Canonical round-trip: the tail must be exactly
			// strconv.FormatInt(n, 10). A StatefulSet never zero-pads, so this
			// name came from somewhere else and the parse is a guess.
			name:    "a zero-padded tail is refused, not parsed",
			podName: "slapd-007",
			wantErr: true,
			because: "hazard 6: a name a StatefulSet would never produce is suspicious",
		},
		{
			name:    "a signed tail is refused",
			podName: "slapd-+7",
			wantErr: true,
			because: "+7 round-trips to \"7\", so accepting it accepts a non-canonical name",
		},
		{
			name:    "a negative tail is refused",
			podName: "slapd--1",
			wantErr: true,
			because: "there is no pod -1; a negative ordinal is a coordinate out of the matrix",
		},
		{
			name:    "a name with no dash is refused",
			podName: "slapd",
			wantErr: true,
			because: "nothing in the name names an ordinal",
		},
		{
			name:    "an empty tail is refused",
			podName: "slapd-",
			wantErr: true,
			because: "the separator is there but the ordinal is not",
		},
		{
			name:    "a non-numeric tail is refused",
			podName: "slapd-readonly",
			wantErr: true,
			because: "an RO pod name must not silently become an ordinal",
		},
		{
			name:    "an empty prefix is refused",
			podName: "-3",
			wantErr: true,
			because: "no StatefulSet is named the empty string",
		},
		{
			name:    "the empty string is refused",
			podName: "",
			wantErr: true,
			because: "an unnamed pod cannot be identified at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := podOrdinal(tc.podName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("podOrdinal(%q) = %d, want an error — %s", tc.podName, got, tc.because)
				}
				if tc.podName != "" && !strings.Contains(err.Error(), tc.podName) {
					t.Errorf("error %q does not name the offending pod %q — the caller holds many pods and must be told which one", err, tc.podName)
				}
				return
			}
			if err != nil {
				t.Fatalf("podOrdinal(%q) errored: %v — %s", tc.podName, err, tc.because)
			}
			if got != tc.want {
				t.Errorf("podOrdinal(%q) = %d, want %d — %s", tc.podName, got, tc.want, tc.because)
			}
		})
	}
}

// runningPod builds a Running remote pod with a primary IP and, when nadIP is
// non-empty, a Multus network-status annotation for NAD "replication-net".
func runningPod(name, podIP, nadIP string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: podIP},
	}
	if nadIP != "" {
		p.Annotations = map[string]string{
			"k8s.v1.cni.cncf.io/network-status": `[{"name":"replication-net","interface":"net1","ips":["` + nadIP + `"]}]`,
		}
	}
	return p
}

func pendingPod(name, podIP string) corev1.Pod {
	p := runningPod(name, podIP, "")
	p.Status.Phase = corev1.PodPending
	return p
}

func ordinalsOf(ps []ldapv1alpha1.DiscoveredProvider) []int32 {
	out := make([]int32, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Ordinal)
	}
	return out
}

func addressesOf(ps []ldapv1alpha1.DiscoveredProvider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Address)
	}
	return out
}

func eqInt32(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProvidersFromPods(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pods         []corev1.Pod
		podRouted    bool
		nadName      string
		wantOrdinals []int32
		wantAddrs    []string
		wantErr      bool
		mustName     []string
		because      string
	}{
		{
			name:         "pod-routed mode takes the pod's primary IP",
			pods:         []corev1.Pod{runningPod("slapd-0", "10.0.0.5", "")},
			podRouted:    true,
			wantOrdinals: []int32{0},
			wantAddrs:    []string{"10.0.0.5"},
			because:      "ADR-016 replication rides the natively routed pod network",
		},
		{
			name:         "multus mode takes the NAD's IP, not the pod IP",
			pods:         []corev1.Pod{runningPod("slapd-0", "10.0.0.5", "192.0.2.7")},
			podRouted:    false,
			nadName:      "replication-net",
			wantOrdinals: []int32{0},
			wantAddrs:    []string{"192.0.2.7"},
			because:      "ADR-007 replication rides net1; the pod IP is the wrong network",
		},
		{
			// The bug in miniature. The old discovery ended with
			// sort.Strings(addresses), so an IP-string order stood in for
			// identity. These three addresses sort to ordinals 2, 0, 1.
			name: "the result is ordered by ordinal, not by address",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.20", ""),
				runningPod("slapd-1", "10.0.0.3", ""),
				runningPod("slapd-2", "10.0.0.100", ""),
			},
			podRouted:    true,
			wantOrdinals: []int32{0, 1, 2},
			wantAddrs:    []string{"10.0.0.20", "10.0.0.3", "10.0.0.100"},
			because:      "sorting by address is what let remote churn permute provider identity",
		},
		{
			name: "input order does not survive; ordinal order does",
			pods: []corev1.Pod{
				runningPod("slapd-2", "10.0.0.3", ""),
				runningPod("slapd-0", "10.0.0.1", ""),
				runningPod("slapd-1", "10.0.0.2", ""),
			},
			podRouted:    true,
			wantOrdinals: []int32{0, 1, 2},
			wantAddrs:    []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
			because:      "the k8s List order is not an identity either",
		},
		{
			// The property that makes identity worth carrying at all: a gap in
			// the surviving set must stay a gap. A positional scheme would
			// report ordinals 0 and 1 here and point the second stanza at the
			// wrong provider.
			name: "a missing middle pod leaves a gap in the ordinals",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.1", ""),
				pendingPod("slapd-1", "10.0.0.2"),
				runningPod("slapd-2", "10.0.0.3", ""),
			},
			podRouted:    true,
			wantOrdinals: []int32{0, 2},
			wantAddrs:    []string{"10.0.0.1", "10.0.0.3"},
			because:      "position is not identity; the gap is the evidence",
		},
		{
			name:         "a pod that is not Running is skipped",
			pods:         []corev1.Pod{pendingPod("slapd-0", "10.0.0.1"), runningPod("slapd-1", "10.0.0.2", "")},
			podRouted:    true,
			wantOrdinals: []int32{1},
			wantAddrs:    []string{"10.0.0.2"},
			because:      "a pod that is not up is not a provider this pass",
		},
		{
			name:         "a Running pod with no address is skipped",
			pods:         []corev1.Pod{runningPod("slapd-0", "", ""), runningPod("slapd-1", "10.0.0.2", "")},
			podRouted:    true,
			wantOrdinals: []int32{1},
			wantAddrs:    []string{"10.0.0.2"},
			because:      "an empty address is not a reachable provider",
		},
		{
			name:         "in multus mode a Running pod without the NAD is skipped",
			pods:         []corev1.Pod{runningPod("slapd-0", "10.0.0.1", ""), runningPod("slapd-1", "10.0.0.2", "192.0.2.8")},
			nadName:      "replication-net",
			wantOrdinals: []int32{1},
			wantAddrs:    []string{"192.0.2.8"},
			because:      "a pod with no net1 IP has no replication address, pod IP notwithstanding",
		},
		{
			name:         "no surviving pods is an empty answer, not an error",
			pods:         []corev1.Pod{pendingPod("slapd-0", "10.0.0.1"), pendingPod("slapd-1", "10.0.0.2")},
			podRouted:    true,
			wantOrdinals: []int32{},
			wantAddrs:    []string{},
			because:      "a peer whose pods are all restarting is legitimately offering nothing",
		},
		{
			name:         "an empty pod list is an empty answer, not an error",
			pods:         nil,
			podRouted:    true,
			wantOrdinals: []int32{},
			wantAddrs:    []string{},
			because:      "nothing to reduce is not a failure to reduce",
		},
		{
			// The refusal hazard 6 demands. Never a positional fallback, and
			// never a silently dropped pod — a short provider set becomes a
			// short stanza set, and olcSyncRepl is written with one atomic
			// Replace, so short means deleted (hazard 2).
			name: "a surviving pod whose name will not parse refuses the whole peer",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.1", ""),
				runningPod("slapd-weird", "10.0.0.2", ""),
			},
			podRouted: true,
			wantErr:   true,
			mustName:  []string{"slapd-weird"},
			because:   "a provider that cannot be identified must not be guessed at or dropped",
		},
		{
			name: "a refusal yields no providers at all, not the parseable subset",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.1", ""),
				runningPod("slapd-1", "10.0.0.2", ""),
				runningPod("slapd-oops", "10.0.0.3", ""),
			},
			podRouted: true,
			wantErr:   true,
			mustName:  []string{"slapd-oops"},
			because:   "returning the parseable two would be the short set hazard 2 forbids",
		},
		{
			// Filters run BEFORE the parse: a pod that is not a provider this
			// pass cannot make the peer refuse. Otherwise one unrelated pod in
			// the namespace — an RO pod, a debug pod matching the selector —
			// would take the peer down whenever it happened to be Pending.
			name: "a non-Running pod with an unparseable name does not refuse",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.1", ""),
				pendingPod("slapd-weird", "10.0.0.2"),
			},
			podRouted:    true,
			wantOrdinals: []int32{0},
			wantAddrs:    []string{"10.0.0.1"},
			because:      "only a pod that would have become a provider can refuse the peer",
		},
		{
			name: "an addressless pod with an unparseable name does not refuse",
			pods: []corev1.Pod{
				runningPod("slapd-0", "10.0.0.1", ""),
				runningPod("slapd-weird", "", ""),
			},
			podRouted:    true,
			wantOrdinals: []int32{0},
			wantAddrs:    []string{"10.0.0.1"},
			because:      "same rule, the other filter",
		},
		{
			// A StatefulSet cannot produce this, so it means the label
			// selector caught two fleets. Choosing one of them would be a
			// guess about which cluster the peer is.
			name: "two surviving pods with the same ordinal refuse, naming both",
			pods: []corev1.Pod{
				runningPod("a-slapd-0", "10.0.0.1", ""),
				runningPod("b-slapd-0", "10.0.0.2", ""),
			},
			podRouted: true,
			wantErr:   true,
			mustName:  []string{"a-slapd-0", "b-slapd-0"},
			because:   "a duplicate ordinal means the selector is wrong, and naming one hides that",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := providersFromPods(tc.pods, tc.podRouted, tc.nadName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("providersFromPods() = %+v, want an error — %s", got, tc.because)
				}
				if len(got) != 0 {
					t.Errorf("providersFromPods() returned %d providers alongside its error, want none — %s", len(got), tc.because)
				}
				for _, n := range tc.mustName {
					if !strings.Contains(err.Error(), n) {
						t.Errorf("error %q does not name %q — the operator holds many pods and must be told which", err, n)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("providersFromPods() errored: %v — %s", err, tc.because)
			}
			if gotOrd := ordinalsOf(got); !eqInt32(gotOrd, tc.wantOrdinals) {
				t.Errorf("ordinals = %v, want %v — %s", gotOrd, tc.wantOrdinals, tc.because)
			}
			if gotAddr := addressesOf(got); !eqStr(gotAddr, tc.wantAddrs) {
				t.Errorf("addresses = %v, want %v — %s", gotAddr, tc.wantAddrs, tc.because)
			}
		})
	}
}
