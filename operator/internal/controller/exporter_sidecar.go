/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	corev1 "k8s.io/api/core/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// exporterContainerName is the name of the slapd exporter sidecar.
const exporterContainerName = "exporter"

// exporterSidecar builds the slapd exporter sidecar (ADR-033 Surface 3) that
// buildStatefulSetSpec appends to the pod's containers when
// desiredMonitoringEnabled(sc) holds, on read-write and read-only pods alike.
//
// Contract (ADR-033 decision 4, "Packaging: its own module, image and
// version line"):
//   - image is spec.images.exporter, defaulting to the slapd-exporter image
//     beside the operator's at the compiled-in exporterDefaultVersion (see
//     exporterImage).
//   - no Command and no Args: the image's entrypoint and the exporter's own
//     defaults (ldapi:///run/openldap/slapd.ldapi, SASL EXTERNAL, :9330) are
//     the whole interface, so the operator and the exporter can be released
//     independently. The operator owns only the pod-side facts below.
//   - mounts the "run" emptyDir at /run/openldap READ-ONLY: connect() needs
//     write permission on the socket inode, not a writable mount.
//   - one container port, 9330 TCP, named "metrics".
//   - securityContext mirrors the slapd container's (no privilege escalation,
//     read-only root FS, drop ALL) and sets no UID or GID: the pod-level
//     securityContext decides those, and the monitor ACL is UID-agnostic.
//   - no readiness, liveness or startup probe: Ready is the AND of all
//     containers, and the exporter must never make a slapd pod NotReady.
//   - resources are spec.monitoring.resources when set, empty otherwise.
//
// ImagePullPolicy is spec.images.exporter.pullPolicy. Once an images.exporter
// object exists, the CRD default stores IfNotPresent there, so that is what an
// otherwise-unset policy reads as. Only with the object absent is the field
// empty here, and then it is left unset — as on the backup Jobs'
// `backup-upload` container — for the API server to default from the tag
// (IfNotPresent for a pinned tag, Always for :latest).
func exporterSidecar(sc *ldapv1alpha1.SlapdCluster, image string) corev1.Container {
	noEsc := false
	roRoot := true
	c := corev1.Container{
		Name:  exporterContainerName,
		Image: image,
		Ports: []corev1.ContainerPort{
			{Name: exporterPortName, ContainerPort: exporterPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: volumeRun, MountPath: runMountPath, ReadOnly: true}},
		// Mirrors the slapd container's securityContext (pinned equal by
		// TestExporterSidecarShape, so the two cannot drift silently). No
		// RunAsUser/RunAsGroup: the pod securityContext decides the identity.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEsc,
			ReadOnlyRootFilesystem:   &roRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if sc != nil {
		c.ImagePullPolicy = sc.Spec.Images.Exporter.PullPolicy
		if sc.Spec.Monitoring.Resources != nil {
			c.Resources = *sc.Spec.Monitoring.Resources.DeepCopy()
		}
	}
	return c
}

// exporterDefaultVersion is the slapd-exporter release this operator runs
// when spec.images.exporter.tag is unset: the bare X.Y.Z image tag of the git
// tag exporter/vX.Y.Z (docs/VERSIONING.md). It is a constant, not the
// operator's tag, so an operator upgrade leaves the sidecar — and therefore
// the pods — unchanged unless this line moves. Bump it only to adopt a newer
// exporter, and only to a released one: the image must exist.
const exporterDefaultVersion = "0.0.1"

// exporterComponent is the exporter image's repository name, swapped in for
// the operator repository's trailing path segment like slapd and slapd-init.
const exporterComponent = "slapd-exporter"

const (
	exporterPortName = "metrics"
	exporterPort     = int32(9330)
)

// exporterImage resolves spec.images.exporter like slapd and init — a missing
// repository is the operator's with its trailing path segment swapped for
// slapd-exporter, falling back to the canonical upstream path when
// OPERATOR_IMAGE is unset — except that a missing tag is the compiled-in
// exporterDefaultVersion, never the operator's tag: the exporter has its own
// version line (ADR-033 decision 4).
func (r *SlapdClusterReconciler) exporterImage(sc *ldapv1alpha1.SlapdCluster) string {
	var img ldapv1alpha1.SlapdImageConfig
	if sc != nil {
		img = sc.Spec.Images.Exporter
	}
	return resolveImageRef(img, defaultDataPlaneRepo(r.OperatorImage, exporterComponent), exporterDefaultVersion)
}

// appendExporterSidecar adds the exporter sidecar to a pod spec when
// spec.monitoring.enabled is on, after every other container so slapd stays
// first. Off or unset leaves the pod spec untouched (ADR-013 amendment
// 2026-10-08: the operator upgrade itself must roll nothing).
func (r *SlapdClusterReconciler) appendExporterSidecar(sc *ldapv1alpha1.SlapdCluster, pod *corev1.PodSpec) {
	if !desiredMonitoringEnabled(sc) {
		return
	}
	pod.Containers = append(pod.Containers, exporterSidecar(sc, r.exporterImage(sc)))
}
