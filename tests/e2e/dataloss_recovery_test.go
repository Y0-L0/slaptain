package e2e_test

// Data-loss-via-replication recovery test. Validates ADR-012's "case 2":
// a multi-pod cluster loses one pod's data (node disk failure, accidental
// PVC deletion) — the empty pod rejoins the mesh and syncrepl restores the
// directory from surviving peers. The operator does NOT re-seed.
//
// The trigger is `kubectl delete pod + pvc` on the persistent fixture. This
// is a more realistic failure simulation than the previous emptyDir-based
// approach (ADR-013 retired the ephemeral fixture for being a non-product).
// The StatefulSet controller recreates the pod from the spec, and because
// the PVCs are gone too, fresh ones are provisioned from volumeClaimTemplates
// — the new pod starts with empty /config, /data, /accesslog volumes, exactly
// the post-disk-failure shape.

import (
	"fmt"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

var _ = Describe("data loss recovery via replication",
	Label("resilience"), Ordered, func() {

	const targetPod = "slapd-1" // pick a non-seed pod to keep this orthogonal to seed semantics

	// PVCs provisioned by the SlapdCluster's volumeClaimTemplates. Names follow
	// the StatefulSet convention `<template>-<sts>-<ordinal>`. Order matters
	// only insofar as we want to delete every volume that holds state — losing
	// just /data wouldn't reproduce real disk failure (config and accesslog
	// would still hold stale state that complicates recovery).
	pvcTemplates := []string{"config", "data", "accesslog"}

	var addedUserDN string
	var preObservedGeneration int64

	// Wall-clock mark taken just before the data-loss event, used to scope the
	// slapd log reads to the recovery window (see the ITS#9398 spec below).
	var lossStartedAt time.Time

	BeforeAll(func(ctx SpecContext) {
		// Add an entry that we'll use as a recovery witness. Seed entries
		// alone would be insufficient — they prove only that the operator-
		// managed bootstrap path is intact. A real recovery should also
		// restore user-added data that came in *after* the initial seed.
		addedUserDN = fmt.Sprintf("uid=dataloss-witness,ou=People,%s", baseDN)
		req := ldap.NewAddRequest(addedUserDN, nil)
		req.Attribute("objectClass", []string{"posixAccount", "shadowAccount", "inetOrgPerson"})
		req.Attribute("cn", []string{"Dataloss Witness"})
		req.Attribute("sn", []string{"Witness"})
		req.Attribute("uid", []string{"dataloss-witness"})
		req.Attribute("uidNumber", []string{"60001"})
		req.Attribute("gidNumber", []string{"60000"})
		req.Attribute("homeDirectory", []string{"/home/dataloss-witness"})
		req.Attribute("loginShell", []string{"/bin/false"})
		ldapAdd(ldapConn, req)

		// Capture the SlapdDatabase's observedGeneration BEFORE the data-loss
		// event. ADR-012's contract: a successful recovery must NOT trigger a
		// re-reconcile of the seed step. If it did, the operator would touch
		// status (even idempotently) and bump observedGeneration. The post-
		// recovery check asserts this number is unchanged.
		var sd ldapv1alpha1.SlapdDatabase
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, &sd)).
			To(Succeed())
		Expect(sd.Status.SeedApplied).To(BeTrue(),
			"precondition: SeedApplied must already be true before the data-loss event")
		preObservedGeneration = sd.Status.ObservedGeneration
	})

	AfterAll(func() {
		// Best-effort cleanup. The witness entry is on every pod via replication
		// by now; deleting from the LB-fronted connection propagates the same way.
		if addedUserDN != "" {
			_ = ldapConn.Del(ldap.NewDelRequest(addedUserDN, nil))
		}
	})

	It("a pod that loses its PVCs recovers the full DIT from peers", func(ctx SpecContext) {
		By("capturing the original " + targetPod + " UID")
		oldPod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, targetPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		oldUID := oldPod.UID

		// Mark the window before anything is destroyed. Pod logs are read back
		// from here in the ITS#9398 spec; a mark taken later would miss the
		// first — busiest — seconds of the refresh.
		lossStartedAt = time.Now().Add(-10 * time.Second)

		By("deleting " + targetPod + "'s PVCs (deletion blocked by pvc-protection finalizer until pod is gone)")
		// k8s adds kubernetes.io/pvc-protection to in-use PVCs. The delete call
		// only marks them with a deletionTimestamp; actual garbage collection
		// happens once no pod references them, which we trigger next by deleting
		// the pod itself.
		for _, tpl := range pvcTemplates {
			pvcName := fmt.Sprintf("%s-%s", tpl, targetPod)
			err := k8sClient.CoreV1().PersistentVolumeClaims(namespace).
				Delete(ctx, pvcName, metav1.DeleteOptions{})
			if err != nil && !kerrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred(), "deleting pvc %s", pvcName)
			}
		}

		By("deleting " + targetPod + " (frees the pvc-protection finalizers; PVCs garbage-collect; STS provisions fresh ones)")
		Expect(k8sClient.CoreV1().Pods(namespace).Delete(ctx, targetPod, metav1.DeleteOptions{})).
			To(Succeed())

		By("waiting for the old PVCs to be fully gone (would block the new pod's volume mount)")
		// ADR-018 R6: report *which* pods still hold the PVC on timeout. Any pod
		// object naming a PVC — a finished Job's pod included — keeps
		// pvc-protection on it, and a bare "timed out after 3 min" gives no clue
		// that a leaked backup/restore Job pod is the reason.
		Eventually(ctx, func() []string {
			var stuck []string
			for _, tpl := range pvcTemplates {
				pvcName := fmt.Sprintf("%s-%s", tpl, targetPod)
				p, err := k8sClient.CoreV1().PersistentVolumeClaims(namespace).
					Get(ctx, pvcName, metav1.GetOptions{})
				if err != nil {
					if kerrors.IsNotFound(err) {
						continue
					}
					stuck = append(stuck, fmt.Sprintf("%s: %v", pvcName, err))
					continue
				}
				// Old PVC still present (either pre-deletion or stuck in
				// terminating). New PVC has no DeletionTimestamp.
				if p.DeletionTimestamp != nil {
					stuck = append(stuck, fmt.Sprintf("%s still terminating, held by: %s",
						pvcName, strings.Join(podsReferencingPVC(ctx, pvcName), ", ")))
				}
				// PVC present without a deletion timestamp = it's been
				// re-created by the STS controller. That's what we want.
			}
			return stuck
		}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(BeEmpty(),
			"old PVCs should garbage-collect and be replaced by fresh ones within 3 min")

		By("waiting for the StatefulSet to recreate " + targetPod + " with a new UID + Ready")
		Eventually(ctx, func() bool {
			p, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, targetPod, metav1.GetOptions{})
			if err != nil {
				return false
			}
			if p.UID == oldUID {
				return false
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return true
				}
			}
			return false
		}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(BeTrue(),
			"%s should come back with a new UID and become Ready within 3 min", targetPod)

		By("connecting directly to the new " + targetPod + " via its per-pod NodePort")
		// dialPodLDAP requires E2E_NODE_IP + E2E_POD_NODEPORT_BASE — set by e2e.sh.
		podConn, podCancel := dialPodLDAP(namespace, targetPod, "")
		defer func() {
			podConn.Close()
			podCancel()
		}()

		By("verifying the seeded OUs converge on " + targetPod + " via replication")
		// Replication needs a moment to drain the change journal from peers
		// into the empty pod. The operator never re-seeds (ADR-012); whatever
		// shows up here came over syncrepl.
		for _, ou := range []string{"People", "Mail", readpwOU} {
			dn := fmt.Sprintf("ou=%s,%s", ou, baseDN)
			Eventually(ctx, func() bool {
				return ldapExists(podConn, dn)
			}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeTrue(),
				"ou=%s should converge to %s via syncrepl after PVC reset", ou, targetPod)
		}

		By("verifying the user-added witness entry also recovers")
		// This is the load-bearing check. If the operator had re-seeded (the
		// wrong behaviour ADR-012 explicitly forbids), only the spec-defined
		// seed entries would come back — the witness wouldn't, because the
		// operator doesn't know about it. Witness convergence => replication
		// is the recovery mechanism, not seed.
		Eventually(ctx, func() bool {
			return ldapExists(podConn, addedUserDN)
		}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(BeTrue(),
			"%s should converge via syncrepl — confirms recovery is replication-driven, "+
				"not a fake re-seed", addedUserDN)

		// The recovered pod came back with a new IP, which peer sites' syncrepl
		// stanzas still do not know about (ADR-016). Same contract as the
		// resilience specs: whoever moves an address waits for the peers.
		waitForCrossSiteReplication(ctx, "the pod-loses-its-PVCs recovery")
	}, NodeTimeout(18*time.Minute))

	// ── ITS#9398: the recovery must not trigger a stale-cookie refresh storm ──
	//
	// A pod rebuilt from the mesh has its accesslog filled by a full refresh,
	// in RECEIVE order. syncprov's minCSN lookup over that log then fails, and
	// the pod answers every consumer of it `err=4096 text=sync cookie is
	// stale` — forever, while every other health signal, CSN convergence
	// included, reads green. `slctl inspect`'s accesslog-mincsn check is the
	// detector (a minCSN sid the contextCSN does not track);
	// docs/ITS-9398-STORM-SIGNATURE.md is the signature.
	//
	// WHAT THIS IS NOT, because both readings were written down here before
	// they were measured:
	//
	//   NOT 2.6-only, and NOT fixed by moving to 2.7. This comment used to say
	//   the spec "justifies running OpenLDAP 2.7" and that on 2.7.1 the count
	//   "should be ~0 everywhere". It is red on 2.7.1: 4563 lines on
	//   2026-10-11, then 2473 and 9237 lines on a three-site mesh on
	//   2026-10-18 — twice in one session, on the same site both times. The
	//   2.6.10 measurements below are real and the upstream fix 414866b8 is
	//   real, but they belong to ITS#9580 (dormancy), which is a different
	//   failure from the one this spec counts. ITS#9398 is fixed in no
	//   release, master included: there is nothing to upgrade to.
	//
	//   NOT the accesslog purge. Checked directly on the affected pod
	//   (2026-10-18): the OLDEST surviving journal entry was written at the
	//   pod's own birth, so the purge had deleted nothing at all, and the
	//   storm reproduced identically with the ADR-037 purge rotation on and
	//   with a single fleet-wide interval. Do not read a red here as a purge
	//   regression.
	//
	// Historical measurements, kept because they bound the threshold. On
	// 2.6.10 the ITS#9580 dormancy loop produced ~14k connections in 27 s and
	// hundreds of millicores per pod — see
	// docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md.
	// Engineered dormancy (an idle-site SID aged past an aggressive
	// accesslogPurge, six armed triggers under live churn,
	// tests/e2e-storm-repro.sh) produced only isolated, self-healing staleness
	// — 3 lines, twice. The threshold of 50 separates that legitimate isolated
	// staleness from the storm (thousands, every time it has fired). Raising
	// it hides the outage rather than fixing it; see ADR-021 and ADR-038.
	It("no 'sync cookie is stale' storm follows the recovery (ITS#9398, ADR-021)",
		func(ctx SpecContext) {
			const needle = "sync cookie is stale"
			// Isolated staleness is legitimate — a consumer can genuinely hold a
			// cookie the provider can no longer serve once, right after the
			// refresh. The storm is three orders of magnitude above this.
			const maxOccurrences = 50

			Expect(lossStartedAt).NotTo(BeZero(),
				"the data-loss spec must have run first (Ordered container)")

			// The message only reaches the log when slapd logs operation results,
			// i.e. the `stats` level (256). Without it the count is trivially
			// zero, which would be a false green — skip loudly instead.
			var sc ldapv1alpha1.SlapdCluster
			Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "slapd"}, &sc)).
				To(Succeed())
			if effectiveLogLevel(&sc)&256 == 0 {
				Skip(fmt.Sprintf("cluster logLevel=%d has no stats bit (256): slapd never logs "+
					"operation results, so %q cannot be observed", effectiveLogLevel(&sc), needle))
			}

			pods := rwPodNames(ctx, "slapd")
			counts, total, problems := countInSlapdLogs(ctx, pods, lossStartedAt, needle)
			Expect(problems).To(BeEmpty(), "could not read slapd logs — an unread log is not a clean log")

			GinkgoLogr.Info("stale-cookie occurrences since the data-loss event",
				"needle", needle, "perPod", counts, "total", total)

			Expect(total).To(BeNumerically("<", maxOccurrences),
				"%d %q lines across %v since the PVC-loss recovery (limit %d). This is the "+
					"ITS#9398 refresh storm: a rebuilt pod's accesslog is filled by a full "+
					"refresh in receive order, and the pod then refuses every consumer of that "+
					"log with a stale cookie while every other health signal — CSN convergence "+
					"included — reads green. Confirm it with `slctl inspect` "+
					"(check accesslog-mincsn: a minCSN sid its contextCSN does not track) and "+
					"see docs/ITS-9398-STORM-SIGNATURE.md. "+
					"NOT 2.6-only and NOT fixed by upgrading: measured on 2.7.1 at 4563 lines "+
					"(2026-10-11) and reproduced twice on a three-site mesh on 2026-10-18 at "+
					"2473 and 9237 lines, on the same site both times. "+
					"NOT caused by the accesslog purge: on the affected pod the oldest journal "+
					"entry was the pod's own birth, so the purge had deleted nothing, and the "+
					"storm reproduced identically with the purge rotation on and off. "+
					"Rebuilding the pod (ADR-012 case 2) clears it; there is no release to "+
					"upgrade to. See ADR-021 and ADR-038.",
				total, needle, pods, maxOccurrences)
		}, NodeTimeout(3*time.Minute))

	It("SlapdDatabase observedGeneration is unchanged across the recovery", func(ctx SpecContext) {
		// Belt-and-braces check on the operator-side promise. ADR-012 says
		// SeedApplied is a one-way latch and the operator does not act on
		// "data missing" detection. If the operator had re-seeded during the
		// recovery, it would have called setStatus → patched status →
		// observedGeneration would tick (independently of spec.generation,
		// since observedGeneration tracks the most recent generation the
		// controller has reconciled). Unchanged means the operator stayed out.
		var sd ldapv1alpha1.SlapdDatabase
		Expect(crdClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dbCRName}, &sd)).
			To(Succeed())

		Expect(sd.Status.SeedApplied).To(BeTrue(),
			"SeedApplied must remain true after recovery (one-way latch, ADR-012)")
		Expect(sd.Status.ObservedGeneration).To(Equal(preObservedGeneration),
			"ObservedGeneration changed (%d → %d) — suggests the operator re-reconciled "+
				"during the data-loss event, which violates ADR-012's one-shot seed contract",
			preObservedGeneration, sd.Status.ObservedGeneration)
	})
})
