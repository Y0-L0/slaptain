package e2e_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// ADR-040: spec.images.select names a data-plane build by what it GOVERNS —
// the storage format its volumes are in, and whether it carries slaptain's
// patches — and drives BOTH the slapd and slapd-init images from that one
// declaration. "Pin both or neither" (ADR-021) stops being a rule a user can
// violate and becomes a property of the API.
//
// WHY THIS SPEC EXISTS AT ALL. The fixture pins spec.images.{slapd,init}.tag
// so that a run tests the images it just built, and an explicit tag beats the
// selector by design. The consequence, found on the mesh cycle of 2026-10-23:
// the suite bypassed the selector entirely and could not have noticed one
// wired to a single image. Clearing the pins is the whole point of this file.
//
// It asserts the StatefulSet TEMPLATE rather than waiting for pods to run. The
// defect in scope is "the operator resolved one image and not the other",
// which the template shows immediately; whether a given pair happens to be
// present in the registry is a different question and not one this spec should
// fail on.
var _ = Describe("ADR-040: the image selector", Label("image-selector"), Ordered, func() {
	const cluster = "slapd"

	var (
		origImages ldapv1alpha1.SlapdImages
		origMain   string
	)

	// containerImages returns the slapd and slapd-init images the operator has
	// currently written into the RW pod template.
	containerImages := func(ctx SpecContext) (string, string) {
		sts, err := k8sClient.AppsV1().StatefulSets(namespace).Get(ctx, cluster, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "read the RW StatefulSet")
		var main, init string
		for _, c := range sts.Spec.Template.Spec.Containers {
			if c.Name == "slapd" {
				main = c.Image
			}
		}
		for _, c := range sts.Spec.Template.Spec.InitContainers {
			if c.Name == "init" {
				init = c.Image
			}
		}
		return main, init
	}

	setImages := func(ctx SpecContext, mutate func(*ldapv1alpha1.SlapdImages)) {
		Eventually(ctx, func() error {
			sc := &ldapv1alpha1.SlapdCluster{}
			if err := crdClient.Get(ctx, nsName(namespace, cluster), sc); err != nil {
				return err
			}
			mutate(&sc.Spec.Images)
			return crdClient.Update(ctx, sc)
		}).WithTimeout(time.Minute).WithPolling(2 * time.Second).Should(Succeed())
	}

	BeforeAll(func(ctx SpecContext) {
		sc := &ldapv1alpha1.SlapdCluster{}
		Expect(crdClient.Get(ctx, nsName(namespace, cluster), sc)).To(Succeed())
		origImages = *sc.Spec.Images.DeepCopy()
		origMain, _ = containerImages(ctx)
		Expect(origMain).NotTo(BeEmpty(), "the RW StatefulSet has no slapd container")

		DeferCleanup(func(ctx SpecContext) {
			By("restoring the fixture's images, and waiting for the fleet to settle")
			setImages(ctx, func(im *ldapv1alpha1.SlapdImages) { *im = *origImages.DeepCopy() })
			Eventually(ctx, func() string {
				main, _ := containerImages(ctx)
				return tagOf(main)
			}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(Equal(tagOf(origMain)),
				"the pod template never went back to the image it started on")
			Eventually(ctx, func() bool {
				return statefulSetReady(k8sClient, namespace, cluster)
			}).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
				"the RW fleet never came back ready on the fixture's images")
		})
	})

	It("is overridden by an explicit tag, which is why the fixture never exercises it", func(ctx SpecContext) {
		if origImages.Slapd.Tag == "" {
			Skip("this fixture does not pin spec.images.slapd.tag, so there is nothing to override")
		}
		By("asking for a flavour the pinned tag does not name")
		setImages(ctx, func(im *ldapv1alpha1.SlapdImages) {
			im.Select = &ldapv1alpha1.SlapdImageSelector{Flavour: "slaptain"}
		})

		// The pin wins and keeps its exact value: rewriting someone's pin would
		// make a pin a suggestion.
		Consistently(ctx, func() string {
			main, _ := containerImages(ctx)
			return main
		}).WithTimeout(30*time.Second).WithPolling(5*time.Second).Should(HaveSuffix(":"+origImages.Slapd.Tag),
			"an explicit tag must beat the selector and be used verbatim")
	})

	It("drives BOTH images from one declaration once the pins are cleared", func(ctx SpecContext) {
		By("clearing the pinned tags so the selector decides")
		setImages(ctx, func(im *ldapv1alpha1.SlapdImages) {
			im.Slapd.Tag = ""
			im.Init.Tag = ""
			im.Select = nil
		})

		// NOT "wait for the image to change": the fixture pins the very tag the
		// operator defaults to, so clearing the pin resolves to the same string
		// and there is nothing to observe. An earlier version of this spec
		// waited for a change here and timed out against a perfectly correct
		// operator — assuming that clearing a pin must be visible is the same
		// mistake as assuming an empty result means a failed one.
		var baseMain, baseInit string
		Eventually(ctx, func() bool {
			baseMain, baseInit = containerImages(ctx)
			return baseMain != "" && baseInit != ""
		}).WithTimeout(time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"the RW pod template never carried both data-plane images")

		By("the default cell carries no suffix — which keeps the charts' tag fallback an identity")
		Expect(tagOf(baseMain)).To(Equal(tagOf(baseInit)),
			"slapd %q and slapd-init %q must take the same tag", baseMain, baseInit)

		By("asking for the patched flavour")
		setImages(ctx, func(im *ldapv1alpha1.SlapdImages) {
			im.Select = &ldapv1alpha1.SlapdImageSelector{Flavour: "slaptain"}
		})

		// The assertion the whole file exists for: ONE field moved, and BOTH
		// images followed it, to the SAME tag. A selector wired to slapd alone
		// looks identical from the CR and is exactly the ADR-021 violation the
		// selector was built to make impossible.
		var gotMain, gotInit string
		Eventually(ctx, func() bool {
			gotMain, gotInit = containerImages(ctx)
			return tagOf(gotMain) != tagOf(baseMain)
		}).WithTimeout(2*time.Minute).WithPolling(5*time.Second).Should(BeTrue(),
			"the slapd image never moved after the selector changed")

		Expect(tagOf(gotMain)).To(HaveSuffix("-slaptain"),
			"flavour: slaptain must resolve to the patched pair, got %q", gotMain)
		Expect(tagOf(gotInit)).To(Equal(tagOf(gotMain)),
			"the init image %q did not follow the selector to the slapd image's tag %q — "+
				"one selector must drive a MATCHED PAIR (ADR-021, ADR-040)", gotInit, gotMain)
	})
})

// tagOf returns the tag portion of a "repository:tag" reference, tolerating a
// registry port in the host part.
func tagOf(ref string) string {
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[colon+1:]
	}
	return ""
}
