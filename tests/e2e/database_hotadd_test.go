package e2e_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	ldap "github.com/go-ldap/ldap/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Adding or removing a SlapdDatabase must reconfigure every pod WITHOUT
// replacing one — the same claim scale_topology_test.go makes for spec.replicas,
// for the last remaining pod-template roll trigger.
//
// slapd needs no restart to gain or lose a database: the operation is a live
// ldap-add against cn=config. The roll is purely our orchestration — the
// directory list rides as DATABASE_DIRS on the init container's env
// (slapdcluster_controller.go:962), so changing the set re-renders the pod
// template and the StatefulSet controller replaces every pod. ADR-013 accepted
// that in May 2026 because the alternatives cost a new RBAC verb, a permanent
// sidecar, or a 60 s async race; its 2026-09-28 amendment retires the
// acceptance, because ADR-018's co-located Job pattern — which did not exist
// when the options were costed — makes the mkdir nearly free.
//
// Two assertions, mirroring the scale spec, because either alone is weak:
//
//	no roll  — the StatefulSet's revision hash is unchanged and every
//	           pre-existing pod keeps its UID. A pod replaced and healthy again
//	           looks identical from every other angle.
//	live add — the database is registered in every pod's cn=config afterwards.
//	           "Nothing restarted" is only interesting if the work happened.
//
// The remove half is the cheaper win and lands first in practice: an orphaned
// directory is disk, not a defect, so a delete needs no filesystem work at all
// and only ever rolled because the env var moved.
//
// Ungated: it needs nothing beyond the standard fixture, and it deletes its own
// database before yielding.
var _ = Describe("database hot add", Label("database-hotadd"), Ordered, func() {
	const (
		hotDB = "hotadd-db"
		// Its OWN tree, not under the fixture's suffix: slapd refuses a
		// database whose suffix is already served by a preceding one, and a
		// subordinate naming context is served by its parent database (the
		// same trap cleanup_policy_test.go documents).
		hotSuffix = "dc=hotadd,dc=hotadd,dc=test"
		// Unique per ADR-003; 100/200 are the fixtures', 300/400 cleanupPolicy's.
		hotRIDBase = int32(500)

		settle = 8 * time.Minute
	)

	var (
		survivors      []string
		uidBefore      map[string]string
		revisionBefore string
	)

	podUIDs := func(ctx SpecContext, pods []string) map[string]string {
		out := map[string]string{}
		for _, p := range pods {
			pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, p, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "reading pod %s", p)
			out[p] = string(pod.UID)
		}
		return out
	}

	revision := func(ctx SpecContext) string {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, "slapd", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return sts.Status.UpdateRevision
	}

	// snapshot records what must survive the change under test.
	snapshot := func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, "slapd"), sc)).To(Succeed())

		replicas := sc.Spec.Replicas
		if replicas == 0 {
			replicas = 1
		}
		survivors = nil
		for i := int32(0); i < replicas; i++ {
			survivors = append(survivors, fmt.Sprintf("slapd-%d", i))
		}
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			survivors = append(survivors, fmt.Sprintf("slapd-readonly-%d", i))
		}

		uidBefore = podUIDs(ctx, survivors)
		revisionBefore = revision(ctx)
		Expect(revisionBefore).NotTo(BeEmpty())
	}

	// assertNoRoll is the whole point of the spec: the pod template did not
	// move, and nothing was replaced. Checked twice — once immediately, and
	// once after a pause, because the operator re-renders the StatefulSet on a
	// watch and the roll can lag the change that triggered it.
	assertNoRoll := func(ctx SpecContext, what string) {
		Expect(revision(ctx)).To(Equal(revisionBefore),
			"the StatefulSet revision changed: %s reached the pod template", what)
		Expect(podUIDs(ctx, survivors)).To(Equal(uidBefore),
			"a pod was replaced: %s must be a live cn=config operation", what)

		Consistently(ctx, func(g Gomega) {
			g.Expect(revision(ctx)).To(Equal(revisionBefore))
			g.Expect(podUIDs(ctx, survivors)).To(Equal(uidBefore))
		}).WithTimeout(45*time.Second).WithPolling(5*time.Second).Should(Succeed(),
			"the roll arrived late — %s still moves the pod template", what)
	}

	AfterAll(func(ctx SpecContext) {
		// Belt for a failed run; the remove spec deletes it on a green one.
		_ = crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: hotDB, Namespace: namespace},
		})
	})

	It("adds a database to every pod without replacing one", func(ctx SpecContext) {
		snapshot(ctx)

		By("creating the SlapdDatabase")
		createCleanupDB(ctx, hotDB, hotSuffix, ldapv1alpha1.CleanupPolicyDelete, true, hotRIDBase)

		By("waiting for it to reach Running")
		Eventually(ctx, func() bool {
			return slapdDatabaseRunning(crdClient, namespace, hotDB)
		}).WithTimeout(settle).WithPolling(5*time.Second).Should(BeTrue(),
			"%s did not reach Running", hotDB)

		By("the database is registered in every pod's cn=config")
		forEachPodConfigConn(ctx, func(pod string, conn *ldap.Conn) {
			Expect(configDBCount(conn, hotSuffix)).To(Equal(1),
				"pod %s does not serve %s", pod, hotSuffix)
		})

		assertNoRoll(ctx, "adding a SlapdDatabase")
	}, SpecTimeout(15*time.Minute))

	It("removes it from every pod without replacing one", func(ctx SpecContext) {
		snapshot(ctx)

		By("deleting the SlapdDatabase CR")
		Expect(crdClient.Delete(ctx, &ldapv1alpha1.SlapdDatabase{
			ObjectMeta: metav1.ObjectMeta{Name: hotDB, Namespace: namespace},
		})).To(Succeed())
		waitCRGone(ctx, hotDB)

		By("the database is gone from every pod's cn=config")
		forEachPodConfigConn(ctx, func(pod string, conn *ldap.Conn) {
			Expect(configDBCount(conn, hotSuffix)).To(Equal(0),
				"pod %s still serves %s", pod, hotSuffix)
		})

		assertNoRoll(ctx, "removing a SlapdDatabase")
	}, SpecTimeout(15*time.Minute))
})
