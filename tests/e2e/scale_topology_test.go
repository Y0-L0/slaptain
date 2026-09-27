package e2e_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Changing spec.replicas must reconfigure the mesh WITHOUT replacing any pod.
//
// This is the operator's headline claim over a chart: adding a replica rewrites
// olcSyncRepl on every existing pod over the wire, where a template-rendering
// deployment would have to roll the StatefulSet to do the same thing. The claim
// was false until 2026-09-28 — LDAP_REPLICAS carried the count into the init
// container's env, and a pod-template field cannot change without replacing
// every pod (docs/reconcile-loop-fixes.md, ADR-013 amendment).
//
// Two assertions, because either alone is weak:
//
//	no roll     — the StatefulSet's revision hash is unchanged and every
//	              pre-existing pod keeps its UID. A pod that was replaced and
//	              came back healthy looks identical from every other angle.
//	live rewrite — each surviving pod's stanza count moves by exactly one, in
//	              both directions. "Nothing restarted" is only interesting if
//	              the reconfiguration actually happened.
//
// The cost of getting this wrong is not local, which is why it went unnoticed:
// under ADR-016 every replaced pod invalidates the syncrepl addresses each
// remote site holds for it (130-268 s measured), so on a mesh a scale-up used
// to hand every peer one rediscovery window per pod.
//
// Ungated: it needs nothing beyond the standard replicated fixture, and it
// restores the original replica count before yielding.
var _ = Describe("scale topology", Label("scale-topology"), Ordered, func() {

	var (
		origReplicas int32
		newPod       string
		dataSuffix   string
		nameStanzas  bool // in-cluster stanzas address peers by headless DNS

		rwPods, roPods []string
		uidBefore      map[string]string
		stanzasBefore  map[string]int
		revisionBefore string
	)

	// podUIDs snapshots the identity of every pod we expect to survive. A UID
	// change is a replacement, whatever the pod looks like afterwards.
	podUIDs := func(ctx SpecContext, pods []string) map[string]string {
		out := map[string]string{}
		for _, p := range pods {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, p, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "reading pod %s", p)
			out[p] = string(pod.UID)
		}
		return out
	}

	stanzaCount := func(ctx SpecContext, pod string) int {
		conn := dialPodConfigEventually(ctx, pod)
		defer conn.Close()
		db, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
		Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
		return len(configAttrValues(conn, db.dn, "olcSyncrepl"))
	}

	stanzasNaming := func(ctx SpecContext, pod, peer string) int {
		conn := dialPodConfigEventually(ctx, pod)
		defer conn.Close()
		db, ok := findMdb(observedMdbDatabases(conn), dataSuffix)
		Expect(ok).To(BeTrue(), "pod %s has no data database %q", pod, dataSuffix)
		n := 0
		for _, s := range configAttrValues(conn, db.dn, "olcSyncrepl") {
			if strings.Contains(s, peer+".slapd-headless.") {
				n++
			}
		}
		return n
	}

	setReplicas := func(ctx SpecContext, n int32) {
		Eventually(ctx, func() error {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, nsName(namespace, "slapd"), sc); err != nil {
				return err
			}
			sc.Spec.Replicas = n
			return crdClient.Update(ctx, sc)
		}).WithTimeout(time.Minute).WithPolling(2*time.Second).Should(Succeed(),
			"setting spec.replicas=%d", n)
	}

	waitReady := func(ctx SpecContext) {
		Eventually(ctx, func() bool {
			return statefulSetReady(k8sClient, namespace, "slapd")
		}).WithTimeout(6*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"StatefulSet slapd never became ready")
	}

	BeforeAll(func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())
		if !sc.Spec.Replication.Enabled || sc.IsConsumerOnly() {
			Skip("cluster writes no in-cluster syncrepl stanzas, so there is no topology to rewrite")
		}
		origReplicas = sc.Spec.Replicas
		if origReplicas < 2 {
			Skip("need at least two pre-existing pods to tell a rewrite from a rebuild")
		}
		// With useForInCluster the peer is addressed by its Multus IP, so the
		// pod name is not in the stanza. The count assertions still hold.
		nameStanzas = !sc.Spec.Replication.Network.UseForInCluster

		sd := &ldapv1alpha1.SlapdDatabase{}
		Expect(crdClient.Get(ctx, nsName(namespace, dbCRName), sd)).To(Succeed())
		dataSuffix = sd.Spec.Suffix
		Expect(dataSuffix).NotTo(BeEmpty())

		newPod = fmt.Sprintf("slapd-%d", origReplicas)

		rwPods = nil
		for i := int32(0); i < origReplicas; i++ {
			rwPods = append(rwPods, fmt.Sprintf("slapd-%d", i))
		}
		roPods = nil
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			roPods = append(roPods, fmt.Sprintf("slapd-readonly-%d", i))
		}

		waitReady(ctx)

		// Restore the fixture even if a spec fails, so the rest of the suite
		// does not inherit a resized cluster.
		DeferCleanup(func(ctx SpecContext) {
			setReplicas(ctx, origReplicas)
			waitReady(ctx)
		})
	}, NodeTimeout(8*time.Minute))

	It("adds a replica without replacing any existing pod", func(ctx SpecContext) {
		survivors := append(append([]string{}, rwPods...), roPods...)
		uidBefore = podUIDs(ctx, survivors)

		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		revisionBefore = sts.Status.UpdateRevision
		Expect(revisionBefore).NotTo(BeEmpty())

		stanzasBefore = map[string]int{}
		for _, p := range survivors {
			stanzasBefore[p] = stanzaCount(ctx, p)
			Expect(stanzasBefore[p]).To(BeNumerically(">", 0),
				"pod %s carries no syncrepl stanza to begin with", p)
		}

		By(fmt.Sprintf("scaling %d -> %d", origReplicas, origReplicas+1))
		setReplicas(ctx, origReplicas+1)
		waitReady(ctx)

		By("the pod template did not move, so nothing was rolled")
		sts, err = k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sts.Status.UpdateRevision).To(Equal(revisionBefore),
			"the StatefulSet revision changed: something replica-dependent reached the pod template")

		Expect(podUIDs(ctx, survivors)).To(Equal(uidBefore),
			"a pre-existing pod was replaced; a scale-up must only ADD a pod")

		By("every surviving pod gained exactly one stanza, written over the wire")
		for _, p := range survivors {
			Eventually(ctx, func() int { return stanzaCount(ctx, p) }).
				WithTimeout(4*time.Minute).WithPolling(10*time.Second).
				Should(Equal(stanzasBefore[p]+1),
					"pod %s did not converge on the new topology", p)
			if nameStanzas {
				Expect(stanzasNaming(ctx, p, newPod)).To(Equal(1),
					"pod %s has no stanza naming the new peer %s", p, newPod)
			}
		}

		By("and the pods were still not replaced while that happened")
		Expect(podUIDs(ctx, survivors)).To(Equal(uidBefore))
	}, SpecTimeout(12*time.Minute))

	It("removes a replica without replacing any existing pod", func(ctx SpecContext) {
		survivors := append(append([]string{}, rwPods...), roPods...)

		By(fmt.Sprintf("scaling %d -> %d", origReplicas+1, origReplicas))
		setReplicas(ctx, origReplicas)
		waitReady(ctx)

		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sts.Status.UpdateRevision).To(Equal(revisionBefore),
			"the StatefulSet revision changed on scale-down")

		Expect(podUIDs(ctx, survivors)).To(Equal(uidBefore),
			"a surviving pod was replaced; a scale-down must only REMOVE a pod")

		By("every surviving pod dropped the departed peer's stanza")
		for _, p := range survivors {
			Eventually(ctx, func() int { return stanzaCount(ctx, p) }).
				WithTimeout(4*time.Minute).WithPolling(10*time.Second).
				Should(Equal(stanzasBefore[p]),
					"pod %s still carries a stanza for the removed peer", p)
			if nameStanzas {
				Expect(stanzasNaming(ctx, p, newPod)).To(BeZero(),
					"pod %s still names the removed peer %s", p, newPod)
			}
		}
	}, SpecTimeout(12*time.Minute))
})
