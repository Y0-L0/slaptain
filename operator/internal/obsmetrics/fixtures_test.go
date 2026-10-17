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

package obsmetrics

// Fixture names shared by this package's tests. The values are arbitrary; what
// matters is that one cluster, database, peer or pod has one spelling across
// the fixtures that refer to it, and that the expected render output spells
// them the same way.
const (
	// healthyCluster's identity.
	testNamespace = "slaptain-testing"
	testCluster   = "slapd"
	testMesh      = "ldap"

	// controlCluster's name and mesh: it shares no value with healthyCluster.
	testControlCluster = "ldap-b"
	testControlMesh    = "mesh-b"

	testDB  = "default"
	testDB2 = "db2"

	testPeerSite2 = "site-2"
	testPeerSite3 = "site-3"
	// testStaticPeer is a peer that does not use discovery.
	testStaticPeer = "static-site"

	testPod0     = "slapd-0"
	testPod1     = "slapd-1"
	testPod2     = "slapd-2"
	testPodGlued = "glued"

	// The ordering fixtures: one name that sorts first, one that sorts last.
	testNameFirst = "alpha"
	testNameLast  = "zulu"
)
