# Demo walkthrough

A copy-paste walk through four slaptain use cases on a single Kubernetes
cluster:

| Step | Use case | Kind |
|------|----------|------|
| 1–3 | Deploy: operator, cluster, schema, database | Helm, `SlapdCluster`, `SlapdSchema`, `SlapdDatabase` |
| 4 | Scale up to three read-write replicas plus one read-only replica | `SlapdCluster` |
| 5 | Back up to S3 | `SlapdBackup` |
| 6 | Restore from that backup | `SlapdRestore` |

---

## Prerequisites

The walkthrough assumes a cluster without slaptain: step 1 installs the
operator.

- A `kubectl` context for a cluster with a default StorageClass and about
  27Gi of provisionable storage: 7Gi per read-write pod (config 1 + data 5 +
  accesslog 1) times three, plus 6Gi for the read-only pod.
- `helm`, `kubectl`, `ldap-utils` and the `aws` CLI on your machine.
- `slctl` built from this repository: `make build-slctl` (binary at
  `bin/slctl`) or `make install-slctl` (installs to `/usr/local/bin`).
- A checkout of this repository. Run every command from this directory:

```bash
cd docs/demo
kubectl create namespace slaptain-demo
```

### The S3 target

Steps 5 and 6 need a bucket. If you have one, put its credentials in
[`00-s3-credentials.yaml`](00-s3-credentials.yaml) and
its endpoint and bucket name in [`05-backup.yaml`](05-backup.yaml).
Otherwise, use the versitygw test fixture. It keeps its objects in an
`emptyDir`, so they last only as long as the pod:

```bash
kubectl create namespace slaptain-s3
kubectl apply -n slaptain-s3 -f ../../tests/resources/versitygw.yaml
kubectl -n slaptain-s3 rollout status deploy/versitygw --timeout=120s
```

Then add the credentials to the demo namespace:

```bash
kubectl apply -f 00-s3-credentials.yaml
```

---

## Step 1 — Install the operator

Skip this step if the cluster already runs a slaptain operator. The
manifests in this walkthrough are written for the operator version in your
checkout; with a different version, a manifest may use a field the installed
CRDs don't know.

Install from the chart in your checkout:

```bash
kubectl apply --server-side -f ../../operator/config/crd/bases/
helm upgrade --install slaptain-operator ../../charts/operator \
  -n slaptain-system --create-namespace
kubectl -n slaptain-system get deploy,pod -l control-plane=controller-manager
```

Helm installs the chart's CRDs only on the first install, never on an upgrade,
so the first command applies them explicitly. The operator image tag comes
from the chart's `appVersion`.

---

## Step 2 — Deploy a single-node cluster

The cluster needs a TLS certificate. `gencert.sh` gets one signed through the
Kubernetes CSR API and stores it as the `slapd-tls` Secret:

```bash
../../tests/gencert.sh -n slaptain-demo -s slapd -H slapd-headless slapd-tls
```

Apply the cluster ([`01-cluster.yaml`](01-cluster.yaml))
and wait for it:

```bash
kubectl apply -f 01-cluster.yaml
kubectl -n slaptain-demo wait --for=jsonpath='{.status.readyReplicas}'=1 \
  statefulset/slapd --timeout=180s
kubectl -n slaptain-demo get slapdcluster slapd     # PHASE: Running
```

A `SlapdCluster` declares only the servers. The operator creates a StatefulSet,
two Services, the PVCs and a `cn=config` admin Secret:

```bash
kubectl -n slaptain-demo get statefulset,svc,secret,pvc
```

---

## Step 3 — Add a schema and a database

Apply the schema ([`02-schema.yaml`](02-schema.yaml)) and
the database ([`03-database.yaml`](03-database.yaml)):

```bash
kubectl apply -f 02-schema.yaml
kubectl apply -f 03-database.yaml
kubectl -n slaptain-demo wait --for=jsonpath='{.status.phase}'=Running \
  slapddatabase/demo-db --timeout=120s
```

If you look at the database right after applying it, it may report `Error`
for a few seconds. Before it can create the database, the operator runs a
short Job that creates the database's directory on each pod's volume; until
that Job finishes, the database counts as not applied on any pod. Once the Job
finishes, the phase changes to `Running` without any action from you.

The operator creates the suffix, the seed entries, the ACLs and the indices.
It also generates the `demo-db-credentials` Secret, which holds the data admin
password and the replication password.

`slctl` reads the endpoint and the credentials from the cluster. List the seed
entries:

```bash
slctl ldapsearch -n slaptain-demo -b "dc=demo,dc=example" -s one dn
```

Add a user with the custom `demoUser` object class, and read it back:

```bash
slctl ldapadd -n slaptain-demo <<'LDIF'
dn: uid=alice,ou=People,dc=demo,dc=example
objectClass: demoUser
cn: Alice Example
sn: Example
uid: alice
mail: alice@demo.example
userPassword: alicePassw0rd
demoContextId: 42
demoQuota: 1048576
LDIF

slctl ldapsearch -n slaptain-demo -b "uid=alice,ou=People,dc=demo,dc=example" \
  cn mail demoContextId demoQuota
```

The ACLs don't let an anonymous bind read `userPassword`, so this search
returns no `userPassword`:

```bash
slctl ldapsearch -n slaptain-demo --anonymous \
  -b "uid=alice,ou=People,dc=demo,dc=example" userPassword cn
```

---

## Step 4 — Scale up

[`04-cluster-ha.yaml`](04-cluster-ha.yaml) changes three
fields on the cluster: `replicas: 3`, `readReplicas: 1`,
`replication.enabled: true`. The database is not touched.

```bash
diff 01-cluster.yaml 04-cluster-ha.yaml
kubectl apply -f 04-cluster-ha.yaml
kubectl -n slaptain-demo wait --for=jsonpath='{.status.readyReplicas}'=3 \
  statefulset/slapd --timeout=300s
kubectl -n slaptain-demo wait --for=jsonpath='{.status.readyReplicas}'=1 \
  statefulset/slapd-readonly --timeout=300s
```

The new pods sync their data from the existing one. `slctl inspect` queries
every pod and checks that they agree:

```bash
slctl inspect -n slaptain-demo slapd
```

Write on pod 0, then read the entry from pod 2 and from the read-only pod:

```bash
slctl ldapadd -n slaptain-demo --pod 0 <<'LDIF'
dn: uid=bob,ou=People,dc=demo,dc=example
objectClass: inetOrgPerson
cn: Bob Example
sn: Example
uid: bob
LDIF

sleep 3
slctl ldapsearch -n slaptain-demo --pod 2 -b "uid=bob,ou=People,dc=demo,dc=example" cn
slctl ldapsearch -n slaptain-demo --pod readonly-0 -b "uid=bob,ou=People,dc=demo,dc=example" cn
```

The read-only pod refuses writes:

```bash
printf 'dn: uid=bob,ou=People,dc=demo,dc=example
changetype: modify
replace: cn
cn: Bob Rejected
' | slctl ldapmodify -n slaptain-demo --pod readonly-0
```

```
ldap_modify: Server is unwilling to perform (53)
```

---

## Step 5 — Back up to S3

Apply the backup ([`05-backup.yaml`](05-backup.yaml)).
The operator runs a Job that dumps the database with `slapcat`, compresses it
and uploads it:

```bash
kubectl apply -f 05-backup.yaml
kubectl -n slaptain-demo wait --for=jsonpath='{.status.phase}'=Completed \
  slapdbackup/demo-backup-1 --timeout=180s
kubectl -n slaptain-demo get slapdbackup demo-backup-1 -o wide
kubectl -n slaptain-demo get slapdbackup demo-backup-1 -o yaml
```

The status records the object key (`path`), its size, and the pod and
`contextCSN` the dump came from.

Check the object in the bucket. `slctl aws` runs your local `aws` CLI with the
endpoint and credentials of the resource named by `--from`:

```bash
slctl aws -n slaptain-demo --from sb/demo-backup-1 \
  s3 ls s3://slaptain-backups/ --recursive --human-readable

KEY=$(kubectl -n slaptain-demo get slapdbackup demo-backup-1 -o jsonpath='{.status.path}')
slctl aws -n slaptain-demo --from sb/demo-backup-1 s3 cp "s3://slaptain-backups/$KEY" - \
  | gzip -dc | head -20
```

The object is a gzipped `slapcat` LDIF.

---

## Step 6 — Restore

Delete Bob:

```bash
slctl ldapdelete -n slaptain-demo "uid=bob,ou=People,dc=demo,dc=example"
```

To watch the restore, run this in a second terminal. The phase goes to
`Restoring` and back to `Running`:

```bash
kubectl -n slaptain-demo get slapdcluster slapd -w
```

Restore the database from the backup
([`06-restore.yaml`](06-restore.yaml)):

```bash
kubectl apply -f 06-restore.yaml
kubectl -n slaptain-demo wait --for=jsonpath='{.status.phase}'=Completed \
  slapdrestore/demo-rollback-1 --timeout=300s
slctl ldapsearch -n slaptain-demo -b "uid=bob,ou=People,dc=demo,dc=example" cn
```

The operator checks the backup, scales the cluster to zero, loads the backup
into every pod with `slapadd`, and scales back up. The cluster is unavailable
while this runs.

---

## Cleanup

Delete the namespace before you uninstall the operator. The operator has to
process the resources' finalizers; without it, the namespace stays in
`Terminating`.

```bash
kubectl delete namespace slaptain-demo
kubectl delete csr slapd-slaptain-demo-csr
```

If you deployed the versitygw fixture, you can keep it for another walk and
just empty it. Its objects live in the pod's `emptyDir`, so a new pod starts
with an empty bucket:

```bash
kubectl -n slaptain-s3 rollout restart deploy/versitygw
kubectl -n slaptain-s3 rollout status deploy/versitygw --timeout=120s
```

Or remove it:

```bash
kubectl delete namespace slaptain-s3
```

If you skipped step 1 because an operator was already installed, stop here.
Otherwise, remove the operator:

```bash
helm uninstall slaptain-operator -n slaptain-system
kubectl delete namespace slaptain-system
```

Helm does not delete CRDs. To remove them too:

```bash
kubectl get crd -o name | grep ldap.chuck-chuck-chuck.net | xargs kubectl delete
```

---

## Further reading

- [Backup & Restore](../BACKUP.md): scheduled backups, retention, restoring
  into a new database.
- [Multi-Site Deployments](../MULTI-SITE.md): replication across clusters.
- [slctl](../slctl.md): the diagnostic CLI.
- [Architecture Decision Records](../adrs/).
