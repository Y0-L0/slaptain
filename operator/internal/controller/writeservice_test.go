/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The rotating write endpoint (ADR-037 decision 2, PURGE-ROTATION-PLAN Phase 4).
//
// ADR-037's third amendment measured what the per-pod phases bought on their
// own: nothing a client can see. Four arms at 100k ops/s put the staggered
// fleet on top of the synchronised fleet and nowhere near the arm where the
// write target did not purge — "the only variable that matters is whether the
// write target purges". So the assertions in this file are the feature. A grid
// that is arithmetically perfect while the writes stay where they were measures
// exactly like no feature at all, with every health signal green.
//
// Three silent failures are pinned hardest, because each of them leaves a
// cluster looking healthy:
//
//  1. the Service naming the pod that is purging (the feature's whole claim);
//  2. the Service naming NOTHING, which is a total write outage and strictly
//     worse than an endpoint that stopped rotating;
//  3. a non-positive requeue, which is a hot loop against the API server from a
//     controller whose real duty cycle is a handful of writes a day.

// testWriteSvcName is the name the rotating write Service must take for the
// shared fixture cluster.
const testWriteSvcName = testClusterName + "-write"

// allReady is the readiness map of a fleet of n pods with nothing wrong.
func allReady(n int32) map[int32]bool {
	m := make(map[int32]bool, n)
	for i := range n {
		m[i] = true
	}
	return m
}

// writeFixture is the cluster the Service-shape assertions are built from: a
// bare reconciler with no client, exactly as headless_publish_test.go does it.
func writeFixture() (*SlapdClusterReconciler, *ldapv1alpha1.SlapdCluster) {
	r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = testClusterName
	sc.Namespace = testNamespace
	sc.Spec.Replicas = 3
	return r, sc
}

// requirePredicate stops a composing test before it hands writeTarget a nil
// predicate to call. The tests that walk the whole grid are about where the
// writer LANDS; a missing predicate is a different and earlier failure, and
// letting it panic inside writeTarget would abort the test binary and hide
// every other case in this file behind one stack trace.
func requirePredicate(t *testing.T) {
	t.Helper()
	if eligibleForWrites(allReady(2), nil) == nil {
		t.Fatal("eligibleForWrites yields no predicate, so writeTarget has nothing to consult and no " +
			"writer can be chosen at all")
	}
}

// podOf spells a StatefulSet pod name the way the StatefulSet controller does,
// which is what statefulset.kubernetes.io/pod-name carries.
func podOf(cluster string, ordinal int32) string {
	return fmt.Sprintf("%s-%d", cluster, ordinal)
}

// The write endpoint has a name of its own, and it must collide with none of
// the four Services the SlapdCluster controller already owns. A collision would
// not fail: the SSA patch carries the same field manager, so the client-facing
// Service would quietly be repointed at a single pod and every read in the
// cluster would land on the one provider that is about to purge.
func TestWriteServiceName(t *testing.T) {
	// Spelled out rather than composed from the constant, for the reason
	// TestWriteServiceSelectorUsesKubernetesPodNameLabel spells out its label:
	// a table built from the same constant as the code agrees with any value.
	if writeServiceSuffix != "-write" {
		t.Fatalf("writeServiceSuffix = %q, want %q — it is the name published to clients, and "+
			"changing it silently strands every client still following the old one", writeServiceSuffix, "-write")
	}

	for _, tc := range []struct {
		name    string
		cluster string
		want    string
		because string
	}{
		{
			name:    "the write endpoint is <cluster>-write",
			cluster: testClusterName,
			want:    testWriteSvcName,
			because: "it is the name clients are told to follow, in the same shape as -headless and -readonly",
		},
		{
			name:    "a cluster whose name already ends in a suffix keeps it",
			cluster: testClusterName + "-readonly",
			want:    testClusterName + "-readonly-write",
			because: "the suffix is appended, never substituted; a name is not parsed",
		},
		{
			name:    testCaseEmpty,
			cluster: "",
			want:    "",
			because: "no cluster is no Service; a bare \"-write\" is refused by the API server too, but reads like a name and sends the reader hunting for a cluster nobody named",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := writeServiceName(tc.cluster); got != tc.want {
				t.Fatalf("writeServiceName(%q) = %q, want %q — %s", tc.cluster, got, tc.want, tc.because)
			}
		})
	}
}

// The name must differ from every Service the cluster controller already
// applies, under the same field manager, into the same namespace.
func TestWriteServiceNameCollidesWithNothing(t *testing.T) {
	r, sc := writeFixture()
	sc.Spec.ReadReplicas = 1

	taken := map[string]string{
		r.headlessService(sc).Name:         "the pod-DNS Service the operator and every syncrepl peer resolve",
		r.clusterIPService(sc).Name:        "the client-facing Service for the whole cluster",
		r.readOnlyService(sc).Name:         "the read-only fleet's client Service",
		r.readOnlyHeadlessService(sc).Name: "the read-only fleet's pod DNS",
	}

	got := writeServiceName(sc.Name)
	if because, clash := taken[got]; clash {
		t.Fatalf("writeServiceName(%q) = %q, which is already %s — an SSA patch under the same field "+
			"manager would repoint it at one pod", sc.Name, got, because)
	}
}

// The selector names exactly one pod, by the label the StatefulSet controller
// stamps on it. Both halves of that sentence are load-bearing: a misspelled key
// that is merely dropped leaves a selector matching every pod in the namespace,
// and a right key with a wrong value leaves one matching nothing.
//
// The cluster's standard labels ride along deliberately — they are the failure
// floor. Lose the pod-name entry and the Service degrades to "every read-write
// pod of this cluster", which is what the client Service already does and is
// survivable; without them the same slip degrades to "every pod in the
// namespace", which is not.
func TestWriteServiceSelector(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ordinal int32
		want    map[string]string
		because string
	}{
		{
			name:    "pod zero",
			ordinal: 0,
			want: map[string]string{
				labelName:               appName,
				labelInstance:           testClusterName,
				labelStatefulSetPodName: testPod0,
			},
			because: "the pod-name label is the only built-in way to select ONE pod of a StatefulSet",
		},
		{
			name:    "a later ordinal",
			ordinal: 2,
			want: map[string]string{
				labelName:               appName,
				labelInstance:           testClusterName,
				labelStatefulSetPodName: testPod2,
			},
			because: "the writer moves every slot, so every ordinal must render",
		},
		{
			name:    "a negative ordinal reads as pod zero",
			ordinal: -1,
			want: map[string]string{
				labelName:               appName,
				labelInstance:           testClusterName,
				labelStatefulSetPodName: testPod0,
			},
			because: "no pod is named slapd--1, and a write Service selecting nothing is a total write outage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := writeServiceSelector(testClusterName, tc.ordinal)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("writeServiceSelector(%q, %d) = %v, want %v — %s",
					testClusterName, tc.ordinal, got, tc.want, tc.because)
			}
		})
	}
}

// The key is spelled out here, deliberately, and not taken from the operator's
// own constant.
//
// statefulset.kubernetes.io/pod-name is Kubernetes' label, not ours: the
// StatefulSet controller stamps it and nothing we write can change what it is
// called. A test that asserts the selector against the same constant the
// selector was built from agrees with any spelling at all — it was tried, and a
// deliberately misspelled constant left the whole file green while the Service
// selected no pod in the cluster. The two Jobs that already co-locate on this
// label (backup_job.go, dirjob.go) are the other users of the real spelling and
// would silently disagree with a drifted constant.
func TestWriteServiceSelectorUsesKubernetesPodNameLabel(t *testing.T) {
	const builtIn = "statefulset.kubernetes.io/pod-name"

	if labelStatefulSetPodName != builtIn {
		t.Fatalf("labelStatefulSetPodName = %q, want %q — this label belongs to the StatefulSet "+
			"controller; any other spelling selects no pod at all", labelStatefulSetPodName, builtIn)
	}
	sel := writeServiceSelector(testClusterName, 1)
	if got := sel[builtIn]; got != testPod1 {
		t.Fatalf("selector[%q] = %q, want %q — the write Service would select every pod of the "+
			"cluster, or none, and the rotation would be invisible either way", builtIn, got, testPod1)
	}
}

// An empty selector in a Service spec matches every pod in the namespace, and a
// nil one is written as an empty one. Neither may ever be produced, for any
// input the caller can reach.
func TestWriteServiceSelectorIsNeverEmpty(t *testing.T) {
	for _, ordinal := range []int32{-7, -1, 0, 1, 15, 1 << 20} {
		sel := writeServiceSelector(testClusterName, ordinal)
		if len(sel) == 0 {
			t.Fatalf("writeServiceSelector(%q, %d) is empty — an empty Service selector matches EVERY "+
				"pod in the namespace, so every write would land wherever kube-proxy chose",
				testClusterName, ordinal)
		}
		if sel[labelStatefulSetPodName] == "" {
			t.Fatalf("writeServiceSelector(%q, %d) carries no %s — without it the Service selects the "+
				"whole read-write fleet and the rotation does nothing at all",
				testClusterName, ordinal, labelStatefulSetPodName)
		}
	}
}

// The feature's entire claim, asserted end to end through the two functions a
// reconciler composes: over every fleet size, every slot of the period and
// every legal slotsFromWriteToPurge, the pod the write Service names is never
// the pod whose purge is current.
//
// purgerotation_test.go pins the same property on writeTarget alone. This one
// pins that nothing between writeTarget's answer and the label a client is
// steered by loses it — an off-by-one in the pod name, an ordinal read from the
// wrong end, a selector built from the slot instead of the writer. Each of
// those would leave the arithmetic correct, the conditions green, and the
// writes landing exactly where ADR-037 measured the 30 s stall.
func TestWriteServiceSelectorNeverNamesThePurgingPod(t *testing.T) {
	requirePredicate(t)

	for n := int32(2); n <= 15; n++ {
		ready := allReady(n)
		for slot := int32(0); slot < n; slot++ {
			// Every legal v: the rule is v mod n != 0, so the sweep covers
			// both signs rather than only the positive half the superseded
			// descending parameterisation admitted.
			for v := -2*n + 1; v <= 2*n; v++ {
				if v%n == 0 {
					continue
				}
				writer, ok := writeTarget(slot, n, v, eligibleForWrites(ready, nil))
				if !ok {
					t.Fatalf("n=%d slot=%d v=%d: no writer chosen with every pod ready — a healthy "+
						"fleet always has one, and no writer means the endpoint stops moving", n, slot, v)
				}
				got := writeServiceSelector(testClusterName, writer)[labelStatefulSetPodName]
				if purging := podOf(testClusterName, slot); got == purging {
					t.Fatalf("n=%d slot=%d v=%d: the write Service selects %q, which is the pod purging "+
						"this slot — ADR-037's third amendment measured the write target's own purge as "+
						"the entire client-visible quantity", n, slot, v, purging)
				}
			}
		}
	}
}

// The write Service is a client endpoint and must be addressable exactly like
// the one clients use today: same ports, same names, same targetPorts, same
// defaulting. A write endpoint on a port nobody dials is an endpoint nobody
// follows, and ADR-037's contract rests on clients following it.
func TestWriteServiceMirrorsTheClientFacingService(t *testing.T) {
	r, sc := writeFixture()

	svc, apply := r.writeService(sc, 1, true)
	if !apply || svc == nil {
		t.Fatalf("writeService(sc, 1, true) = %v, %v; want a Service and true — there is no write "+
			"endpoint to follow", svc, apply)
	}
	ref := r.clusterIPService(sc)

	if svc.TypeMeta != ref.TypeMeta {
		t.Fatalf("TypeMeta = %+v, want %+v — applyConfiguration converts through unstructured, and an "+
			"object with no GVK has nothing to convert to", svc.TypeMeta, ref.TypeMeta)
	}
	if svc.Name != testWriteSvcName {
		t.Fatalf("Name = %q, want %q", svc.Name, testWriteSvcName)
	}
	if svc.Namespace != testNamespace {
		t.Fatalf("Namespace = %q, want %q", svc.Namespace, testNamespace)
	}
	if !reflect.DeepEqual(svc.Spec.Ports, ref.Spec.Ports) {
		t.Fatalf("Ports = %+v, want %+v — the write endpoint must answer on the same ports as the "+
			"Service it diverts writes away from", svc.Spec.Ports, ref.Spec.Ports)
	}
	wantSel := writeServiceSelector(sc.Name, 1)
	if !reflect.DeepEqual(svc.Spec.Selector, wantSel) {
		t.Fatalf("Selector = %v, want %v — the Service must select the writer and only the writer",
			svc.Spec.Selector, wantSel)
	}
}

// Ports default to 389/636 and follow spec.service when the user moved them,
// as clusterIPService and readOnlyService both do.
func TestWriteServicePorts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		svc        ldapv1alpha1.SlapdServiceConfig
		wantLDAP   int32
		wantLDAPS  int32
		wantType   corev1.ServiceType
		because    string
		targetLDAP string
	}{
		{
			name:       "unset defaults to the standard LDAP ports and ClusterIP",
			svc:        ldapv1alpha1.SlapdServiceConfig{},
			wantLDAP:   389,
			wantLDAPS:  636,
			wantType:   corev1.ServiceTypeClusterIP,
			targetLDAP: portNameLDAP,
			because:    "a client that already dials :389 must not need a second port to write",
		},
		{
			name: "declared ports and type are honoured",
			svc: ldapv1alpha1.SlapdServiceConfig{
				Type: corev1.ServiceTypeNodePort, LDAPPort: 1389, LDAPSPort: 1636,
			},
			wantLDAP:   1389,
			wantLDAPS:  1636,
			wantType:   corev1.ServiceTypeNodePort,
			targetLDAP: portNameLDAP,
			because:    "a cluster exposed as NodePort needs its write endpoint reachable by the clients it just told to follow it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sc := writeFixture()
			sc.Spec.Service = tc.svc

			svc, apply := r.writeService(sc, 0, true)
			if !apply || svc == nil {
				t.Fatalf("writeService(sc, 0, true) = %v, %v; want a Service and true — there is no "+
					"write endpoint to follow", svc, apply)
			}
			if svc.Spec.Type != tc.wantType {
				t.Fatalf("Type = %q, want %q — %s", svc.Spec.Type, tc.wantType, tc.because)
			}
			if len(svc.Spec.Ports) != 2 {
				t.Fatalf("got %d ports, want 2 (ldap, ldaps)", len(svc.Spec.Ports))
			}
			want := []corev1.ServicePort{
				{Name: portNameLDAP, Port: tc.wantLDAP, TargetPort: intstr.FromString(portNameLDAP), Protocol: corev1.ProtocolTCP},
				{Name: portNameLDAPS, Port: tc.wantLDAPS, TargetPort: intstr.FromString(portNameLDAPS), Protocol: corev1.ProtocolTCP},
			}
			if !reflect.DeepEqual(svc.Spec.Ports, want) {
				t.Fatalf("Ports = %+v, want %+v — %s", svc.Spec.Ports, want, tc.because)
			}
		})
	}
}

// A client-facing Service must NOT publish NotReady addresses. The headless
// Services do, because the operator and the syncrepl peers have to reach a sick
// pod to converge it (ADR-030, headless_publish_test.go). This one is the
// opposite case: a pod that does not answer LDAP is the last pod writes should
// be steered to, and the rotation's whole premise is that the endpoint names a
// provider that can serve.
func TestWriteServiceDoesNotPublishNotReadyAddresses(t *testing.T) {
	r, sc := writeFixture()

	svc, apply := r.writeService(sc, 1, true)
	if !apply || svc == nil {
		t.Fatalf("writeService(sc, 1, true) = %v, %v; want a Service and true — there is no write "+
			"endpoint to follow", svc, apply)
	}
	if svc.Spec.PublishNotReadyAddresses {
		t.Fatal("the write Service publishes NotReady addresses — it would steer writes at a pod that " +
			"is not answering LDAP, which is worse than steering them at one that is about to purge")
	}
}

// spec.service's LoadBalancer fields are not copied. A LoadBalancer IP is one
// address and two Services cannot both claim it; readOnlyService leaves them
// out for exactly this reason, and the write Service is the second client
// endpoint in the same namespace.
func TestWriteServiceClaimsNoLoadBalancerAddress(t *testing.T) {
	r, sc := writeFixture()
	sc.Spec.Service = ldapv1alpha1.SlapdServiceConfig{
		Type:                     corev1.ServiceTypeLoadBalancer,
		LoadBalancerIP:           testPodIP1,
		LoadBalancerSourceRanges: []string{"192.0.2.0/24"},
	}

	svc, apply := r.writeService(sc, 1, true)
	if !apply || svc == nil {
		t.Fatalf("writeService(sc, 1, true) = %v, %v; want a Service and true — there is no write "+
			"endpoint to follow", svc, apply)
	}
	if svc.Spec.LoadBalancerIP != "" {
		t.Fatalf("LoadBalancerIP = %q — the cluster Service already claims that address, and two "+
			"Services cannot both have it", svc.Spec.LoadBalancerIP)
	}
	if len(svc.Spec.LoadBalancerSourceRanges) != 0 {
		t.Fatalf("LoadBalancerSourceRanges = %v, want none", svc.Spec.LoadBalancerSourceRanges)
	}
}

// The no-writer case must not be buildable.
//
// writeTarget reports (0, false) when nothing is eligible, and 0 is also a
// perfectly ordinary pod-0. Taking the ordinal without the verdict would make
// that state representable, and the Service built from it would steer every
// write in the cluster onto pod 0 — on the slot where pod 0 is the pod purging,
// that is exactly the 30 s write stall ADR-037's third amendment measured as the
// entire client-visible quantity, arriving silently with every health signal
// green.
//
// So the pair travels together and ok false yields no Service at all. The
// caller then applies nothing and the selector stays where it was, which is
// PURGE-ROTATION-PLAN's "Known risks" requirement: operator downtime degrades to
// today, but the write Service must never be left selecting nothing. A guard in
// the reconciler would do the same job right up until somebody refactors the
// reconciler; an unrepresentable state survives that.
func TestWriteServiceRefusesToBuildWithoutAWriter(t *testing.T) {
	r, sc := writeFixture()

	for _, writer := range []int32{0, 1, 2} {
		svc, apply := r.writeService(sc, writer, false)
		if apply {
			t.Fatalf("writeService(sc, %d, false) reports a Service to apply — no pod was eligible, "+
				"and applying one would repoint every write at pod %d, which may be the pod purging",
				writer, writer)
		}
		if svc != nil {
			t.Fatalf("writeService(sc, %d, false) = %+v, want nil — there is no writer to name",
				writer, svc)
		}
	}
}

// A nil cluster must not panic the reconciler, like every other pure builder in
// this package.
func TestWriteServiceNilClusterIsInert(t *testing.T) {
	r := &SlapdClusterReconciler{ClusterDomain: testClusterDomain}
	svc, apply := r.writeService(nil, 0, true)
	if svc != nil || apply {
		t.Fatalf("writeService(nil, 0, true) = %+v, %v; want nil, false — there is no cluster to "+
			"build a Service for", svc, apply)
	}
}

// The requeue that drives the rotation. It is STRICTLY POSITIVE for every
// input, and that is the hardest rule in the file: zero or negative is a hot
// loop against the API server, from a controller that otherwise does a handful
// of writes a day.
//
// The grid is the absolute, epoch-anchored, whole-second one rotationSlot reads
// and purgePhase writes (ADR-037's first amendment: deriving the next run from
// absolute time is what retires decision 5a's three drift sources). So the slot
// length truncates to whole seconds here exactly as it does there — a requeue
// computed on a finer grid than the one being rotated wakes the controller just
// beside every boundary it meant to catch, forever, and silently.
func TestNextSlotBoundary(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()

	for _, tc := range []struct {
		name    string
		now     time.Time
		t       time.Duration
		want    time.Duration
		because string
	}{
		{
			name: "exactly on a boundary yields a whole slot, never zero",
			now:  epoch, t: 10 * time.Minute, want: 10 * time.Minute,
			because: "a slot that has just begun has a whole slot left in it; zero would requeue instantly and land here again",
		},
		{
			name: "a nanosecond into a slot yields all but that nanosecond",
			now:  epoch.Add(time.Nanosecond), t: 10 * time.Minute, want: 10*time.Minute - time.Nanosecond,
			because: "the boundary is absolute, so the answer is the distance to it and not a rounded cadence",
		},
		{
			name: "a nanosecond before a boundary still yields a positive duration",
			now:  epoch.Add(10*time.Minute - time.Nanosecond), t: 10 * time.Minute, want: time.Nanosecond,
			because: "the tightest requeue the grid can ask for is still a requeue, not a hot loop",
		},
		{
			name: "the grid repeats, so the slot the caller is in does not matter",
			now:  epoch.Add(7*10*time.Minute + 4*time.Minute), t: 10 * time.Minute, want: 6 * time.Minute,
			because: "boundaries are k*t from the epoch, not from whenever the operator started",
		},
		{
			name: "a one-second grid is the finest the server can express",
			now:  epoch.Add(1500 * time.Millisecond), t: time.Second, want: 500 * time.Millisecond,
			because: "olcAccessLogPurge stores whole seconds, and one second is the floor the rotation gate admits",
		},
		{
			name: "a slot length truncates to whole seconds, as rotationSlot does",
			now:  epoch, t: 90500 * time.Millisecond, want: 90 * time.Second,
			because: "two grids, one from the requeue and one from the slot lookup, would disagree by half a second every cycle",
		},
		{
			name: "before the epoch the grid is still continuous",
			now:  time.Unix(-1, 0).UTC(), t: 10 * time.Minute, want: time.Second,
			because: "a floor rather than Go's truncation; the alternative makes the grid discontinuous at exactly one point",
		},
		{
			name: "a sub-second slot length has no grid",
			now:  epoch.Add(3 * time.Second), t: 500 * time.Millisecond, want: noGridRequeue,
			because: "rotationSlot reads the same length as no grid at all; the two must not disagree about whether one exists",
		},
		{
			name: "a zero slot length has no grid",
			now:  epoch.Add(3 * time.Second), t: 0, want: noGridRequeue,
			because: "the rotation gate already refused this with a condition; what matters here is that the defensive path costs one requeue, not a loop",
		},
		{
			name: "a negative slot length has no grid",
			now:  epoch.Add(3 * time.Second), t: -5 * time.Minute, want: noGridRequeue,
			because: "a negative modulus is the classic route to a zero requeue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextSlotBoundary(tc.now, tc.t)
			if got != tc.want {
				t.Fatalf("nextSlotBoundary(%s, %s) = %s, want %s — %s",
					tc.now.UTC(), tc.t, got, tc.want, tc.because)
			}
		})
	}
}

// Strict positivity, swept rather than sampled. Any zero or negative answer is
// a controller spinning on the API server.
func TestNextSlotBoundaryIsAlwaysPositive(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	slots := []time.Duration{
		-time.Hour, 0, time.Nanosecond, 999 * time.Millisecond,
		time.Second, 90500 * time.Millisecond, 10 * time.Minute, 6 * time.Hour, 24 * time.Hour,
	}
	offsets := []time.Duration{
		-time.Hour, -time.Nanosecond, 0, time.Nanosecond, time.Second,
		10*time.Minute - time.Nanosecond, 10 * time.Minute, 7*6*time.Hour + 17*time.Second,
	}

	for _, slot := range slots {
		for _, off := range offsets {
			now := epoch.Add(off)
			if got := nextSlotBoundary(now, slot); got <= 0 {
				t.Fatalf("nextSlotBoundary(%s, %s) = %s — a non-positive RequeueAfter makes the "+
					"controller spin on the API server", now.UTC(), slot, got)
			}
		}
	}
}

// The requeue and the slot lookup must read ONE grid. Waking at now+boundary
// has to land in the next slot of the period, every time — that is the whole
// reason the requeue is computed rather than set to a fixed cadence, and a
// disagreement between the two would show up as a rotation that occasionally
// skips a pod or repoints twice in a row.
func TestNextSlotBoundaryLandsOnTheNextSlot(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()

	for _, slot := range []time.Duration{time.Second, 10 * time.Minute, 90500 * time.Millisecond, 6 * time.Hour} {
		for _, off := range []time.Duration{0, time.Nanosecond, 17 * time.Second, 5*time.Minute + 1} {
			for n := int32(2); n <= 5; n++ {
				now := epoch.Add(off)
				next := now.Add(nextSlotBoundary(now, slot))
				want := (rotationSlot(now, slot, n) + 1) % n
				if got := rotationSlot(next, slot, n); got != want {
					t.Fatalf("slot=%s off=%s n=%d: waking at now+nextSlotBoundary lands in slot %d, "+
						"want %d — the requeue and the slot lookup are reading different grids",
						slot, off, n, got, want)
				}
			}
		}
	}
}

// Eligibility is everything the grid does not know, and it is the one place
// anything beyond the grid may speak (ADR-037 decision 4).
//
// Readiness first: a pod ABSENT from the map is not ready. Unreadable evidence
// never counts in favour — ADR-025's rule, carried onto this surface. The
// opposite default is the attractive one and it is wrong: it would steer writes
// at precisely the pods the operator could not assess.
//
// The authorship term second: ADR-036 makes a rebuilt pod that has not
// recovered its own authorship read-only, and writes steered there would meet
// the unwillingToPerform the gate exists to produce. ADR-036 is still PROPOSED
// and nothing populates the map, so the term must be inert today and must not
// need editing the day the gate lands.
func TestEligibleForWrites(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ready        map[int32]bool
		heldReadOnly map[int32]bool
		ordinal      int32
		want         bool
		because      string
	}{
		{
			name:  "a ready pod under no authorship hold may take writes",
			ready: allReady(3), ordinal: 1, want: true,
			because: "the ordinary case, and the one the whole rotation runs on",
		},
		{
			name:  "a pod the operator could not assess is not ready",
			ready: map[int32]bool{0: true}, ordinal: 2, want: false,
			because: "absence is not readiness; unreadable evidence never counts in favour (ADR-025)",
		},
		{
			name:  "a pod reported not ready is not eligible",
			ready: map[int32]bool{0: true, 1: false}, ordinal: 1, want: false,
			because: "an unready pod does not answer LDAP, so writes steered there do not land at all",
		},
		{
			name:  "a nil readiness map makes nobody eligible",
			ready: nil, ordinal: 0, want: false,
			because: "a pass that read no readiness has no grounds to move the endpoint; writeTarget then reports no writer and the selector stays put",
		},
		{
			name:  "an empty authorship-hold map changes nothing",
			ready: allReady(3), heldReadOnly: map[int32]bool{}, ordinal: 1, want: true,
			because: "ADR-036 is Proposed and populates nothing; the seam must be inert, never a dependency that blocks",
		},
		{
			name:  "a pod held read-only by the authorship gate may not take writes",
			ready: allReady(3), heldReadOnly: map[int32]bool{1: true}, ordinal: 1, want: false,
			because: "a pod ADR-036 is holding would answer the write with unwillingToPerform",
		},
		{
			name:  "a hold on another pod does not reach this one",
			ready: allReady(3), heldReadOnly: map[int32]bool{2: true}, ordinal: 1, want: true,
			because: "the gate is per pod; one held pod must not empty the candidate set",
		},
		{
			name:  "a false entry in the hold map is not a hold",
			ready: allReady(3), heldReadOnly: map[int32]bool{1: false}, ordinal: 1, want: true,
			because: "presence of a key is not the signal; the value is",
		},
		{
			name:  "a negative ordinal is not eligible",
			ready: allReady(3), ordinal: -1, want: false,
			because: "no pod has that ordinal, and the predicate must answer rather than panic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pred := eligibleForWrites(tc.ready, tc.heldReadOnly)
			if pred == nil {
				t.Fatal("eligibleForWrites returned a nil predicate — writeTarget would panic calling it")
			}
			if got := pred(tc.ordinal); got != tc.want {
				t.Fatalf("eligibleForWrites(...)(%d) = %v, want %v — %s",
					tc.ordinal, got, tc.want, tc.because)
			}
		})
	}
}

// The ADR-036 seam is inert, swept over the whole grid rather than sampled.
// Nothing populates heldReadOnly today, so a nil map and an empty map must
// choose the same writer for every fleet size, slot and offset — otherwise the
// rotation's behaviour depends on which of two spellings of "nothing" the
// caller happened to pass, which is the kind of difference that only ever shows
// up in production.
func TestEligibleForWritesAuthorshipSeamIsInert(t *testing.T) {
	requirePredicate(t)

	empty := map[int32]bool{}
	for n := int32(2); n <= 8; n++ {
		ready := allReady(n)
		for slot := int32(0); slot < n; slot++ {
			for d := int32(1); d < n; d++ {
				gotNil, okNil := writeTarget(slot, n, d, eligibleForWrites(ready, nil))
				gotEmpty, okEmpty := writeTarget(slot, n, d, eligibleForWrites(ready, empty))
				if gotNil != gotEmpty || okNil != okEmpty {
					t.Fatalf("n=%d slot=%d d=%d: nil heldReadOnly chose (%d, %v) and an empty one chose "+
						"(%d, %v) — ADR-036's gate is Proposed and populates neither, so it must change "+
						"no outcome at all", n, slot, d, gotNil, okNil, gotEmpty, okEmpty)
				}
			}
		}
	}
}

// WriteEndpointRotating is observation only: it names the writer and the next
// rotation. It gates nothing — RotationConfigured carries whether the rotation
// is usable, AccesslogPurgeConverged carries whether a pod took its purge
// value, and this one carries where the writes are being sent.
func TestWriteRotationCondition(t *testing.T) {
	_, sc := writeFixture()
	sc.Generation = 11
	next := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	cond := writeRotationCondition(sc, 2, true, next, 0)
	if cond.Type != writeEndpointCondition {
		t.Fatalf("Type = %q, want %q — the condition is named positively, like every other one this "+
			"operator publishes", cond.Type, writeEndpointCondition)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("Status = %q, want True — a writer was chosen, which is the good case", cond.Status)
	}
	if cond.Reason != reasonWriterSelected {
		t.Fatalf("Reason = %q, want %q", cond.Reason, reasonWriterSelected)
	}
	if cond.ObservedGeneration != 11 {
		t.Fatalf("ObservedGeneration = %d, want 11 — a stale verdict must be recognisable as stale",
			cond.ObservedGeneration)
	}
	if cond.LastTransitionTime.IsZero() {
		t.Fatal("LastTransitionTime is zero; setCondition and every reader of it rely on it")
	}
	for _, want := range []string{testPod2, "12:00"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q does not mention %q — it must name the pod writes are being sent to "+
				"and when that moves, or it is not observation of anything", cond.Message, want)
		}
	}
}

// The distinction this condition exists to carry. writeTarget reports (0,
// false) when nothing is eligible, and a writer of 0 is also a perfectly
// ordinary pod-0. If the two render alike, a reader of a cluster with no
// eligible provider sees "writes are going to slapd-0" — and the Service is in
// fact still pointing wherever it pointed before, which may be any pod at all.
func TestWriteRotationConditionDistinguishesNoWriterFromPodZero(t *testing.T) {
	_, sc := writeFixture()
	next := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	chosen := writeRotationCondition(sc, 0, true, next, 0)
	none := writeRotationCondition(sc, 0, false, next, 0)

	if chosen.Status == none.Status && chosen.Reason == none.Reason {
		t.Fatalf("a chosen pod-0 and a no-writer verdict both render as %s/%s — the second means the "+
			"selector was deliberately left where it was, and must not read as a repoint",
			none.Status, none.Reason)
	}
	if none.Status != metav1.ConditionFalse {
		t.Fatalf("no-writer Status = %q, want False", none.Status)
	}
	if none.Reason != reasonNoEligibleWriter {
		t.Fatalf("no-writer Reason = %q, want %q", none.Reason, reasonNoEligibleWriter)
	}
	if strings.Contains(none.Message, testPod0) {
		t.Fatalf("no-writer message %q names %s — no pod was chosen, and naming the zero value as the "+
			"writer is how a total outage reads as a healthy rotation", none.Message, testPod0)
	}
	if !strings.Contains(none.Message, "unchanged") {
		t.Fatalf("no-writer message %q does not say the endpoint is unchanged — the one thing a reader "+
			"needs to know here is that nothing moved and writes are still going somewhere", none.Message)
	}
}

// A next-rotation instant that is not known must be said in words. Rendering a
// zero time produces "0001-01-01T00:00:00Z" in a user-visible condition, which
// reads as a bug in the operator rather than as an absence of information.
func TestWriteRotationConditionHandlesAnUnknownNextRotation(t *testing.T) {
	_, sc := writeFixture()

	cond := writeRotationCondition(sc, 1, true, time.Time{}, 0)
	if strings.Contains(cond.Message, "0001") {
		t.Fatalf("message %q renders the zero time; say the next rotation is unknown instead",
			cond.Message)
	}
	if cond.Message == "" {
		t.Fatal("message is empty; a condition that says nothing observes nothing")
	}
}

// A nil cluster must not panic, like every other pure resolver in this package.
func TestWriteRotationConditionNilClusterIsInert(t *testing.T) {
	cond := writeRotationCondition(nil, 0, false, time.Time{}, 0)
	if cond.Type != writeEndpointCondition {
		t.Fatalf("Type = %q, want %q", cond.Type, writeEndpointCondition)
	}
	if cond.ObservedGeneration != 0 {
		t.Fatalf("ObservedGeneration = %d, want 0 — there is no cluster to have a generation",
			cond.ObservedGeneration)
	}
}
