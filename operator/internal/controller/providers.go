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

// Peer discovery returns provider IDENTITY, not bare addresses — ADR-031.
//
// Until ADR-031 discovery returned a list of IP strings sorted with
// sort.Strings. That list carried no identity at all: a remote pod that was
// briefly not Running dropped out, an IP reassignment permuted the order, and
// every consumer that indexed into it (the `(localOrdinal + k) % remoteN`
// diagonal) silently pointed at a different provider than it had the pass
// before. ADR-031 keys a syncrepl RID on the provider, so position-as-identity
// is not merely untidy, it is a renumber on every remote blip.
//
// The identity is the remote pod's StatefulSet ordinal, which is declared and
// stable, and the only place it can be read from is the pod's name. Hence
// RID-PLAN hazard 6: parse the ordinal, never infer it from list position, and
// refuse a name that will not parse rather than falling back to position.

// podOrdinal recovers a StatefulSet pod's ordinal from its name.
//
// A StatefulSet names its pods `<statefulset>-<ordinal>`, so the ordinal is
// the text after the LAST dash: "slapd-3" is 3 and "my-cluster-slapd-11" is
// 11.
//
// The parse is canonical, not lenient: the tail must be exactly the decimal
// rendering of the result, so "slapd-007", "slapd-+7" and "slapd--1" are
// refusals rather than parses. A name a StatefulSet would never produce is
// evidence that the pod is not what the caller thinks it is, and under hazard
// 6 the answer to that is a refusal, never a guess.
//
// Refused: a name with no dash, an empty tail, a non-numeric tail, an empty
// prefix ("-3"), and the empty string. Every error names the offending pod
// name, because the caller's error is about one pod among many.
//
// "slapd--1" is on that list too, and it is the one case last-dash-wins does
// NOT refuse on its own: the last dash splits it into prefix "slapd-" and tail
// "1", which parses canonically to 1. The refusal comes from the prefix rather
// than the tail — a Kubernetes object name cannot end in a dash, so no
// StatefulSet is named "slapd-" and no pod of one is named "slapd--1". The
// prefix must therefore be non-empty AND not itself end in a dash.
func podOrdinal(podName string) (int32, error) {
	panic("not implemented: ADR-031 M1")
}

// providersFromPods reduces a remote pod list to the providers a peer offers.
//
// Two filters, then a refusal, then an order:
//
//   - A pod not in corev1.PodRunning is not a provider this pass, and is
//     skipped silently. (Its RID does not move when it comes back: ADR-031
//     keys the remote slot on the peer SITE, so a pod that is away costs a
//     reselection of `provider=` and nothing else.)
//   - A pod whose replication address is empty is skipped too. The address is
//     pod.Status.PodIP when podRouted, else the Multus net1 IP for nadName.
//   - A pod that survives BOTH filters but whose name will not parse is a
//     refusal: the function returns an error naming that pod and NO providers
//     at all. It is never dropped silently, because a dropped provider is a
//     short stanza set and `olcSyncRepl` is written with one atomic Replace —
//     a short set deletes (hazard 2). And it is never given a positional
//     ordinal, because that is exactly the inference hazard 6 forbids.
//
// The result is sorted ASCENDING BY ORDINAL — by identity, not by address.
// Sorting by address is what made the old list permute under remote churn.
//
// Two surviving pods claiming the same ordinal is a refusal naming both: a
// StatefulSet cannot produce that, so it means the label selector caught pods
// from more than one fleet, and picking one of them would be a guess.
//
// No surviving pods is (nil, nil) — an empty answer, not an error. A peer whose
// pods are all restarting is legitimately offering nothing right now; the
// caller's own short-set handling decides what that means.
func providersFromPods(pods []corev1.Pod, podRouted bool, nadName string) ([]ldapv1alpha1.DiscoveredProvider, error) {
	panic("not implemented: ADR-031 M1")
}
