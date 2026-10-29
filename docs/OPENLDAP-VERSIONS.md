# OpenLDAP versions and image tags

slaptain ships three slapd image pairs. The plain tag is **OpenLDAP 2.7.1**;
the `-ol26` tag is the previous **OpenLDAP 2.6** build from Debian's package;
the `-ol26src` tag is that same 2.6 built from Debian's *source* package with
local patches applied. The decision and its reasoning are in
[ADR-021](adrs/adr-021-openldap-2.7-dual-images.md).

## Which image is which

**Choose with `spec.images.select`, not by typing a tag.** Since 0.5.0 the
operator takes two axes — the on-disk format a build can open, and whether it
carries slaptain's patches — and resolves both data-plane images from them, so
the pair cannot be mismatched:

```yaml
spec:
  images:
    select:
      storageFormat: lmdb1.0   # lmdb1.0 = OpenLDAP 2.7 (default) | lmdb0.9 = 2.6
      flavour: pure            # pure (default) | slaptain
```

The tags below are what those selections resolve to. An explicit
`spec.images.slapd.tag` still wins and is used verbatim.

| selection | image tag | OpenLDAP, and where the binary comes from |
|---|---|---|
| `lmdb1.0` + `pure` | `slapd:<tag>` + `slapd-init:<tag>` | **the default.** 2.7.1, our build from pristine upstream with Debian's packaging forward-ported onto it. None of our patches (`slapd -VV` ends `+pure<N>`) |
| `lmdb1.0` + `slaptain` | `:<tag>-ol2.7.2-slaptain` | the same build, with our patches (`+slaptain<N>`). Needed by `spec.rotation`'s purge phases |
| `lmdb0.9` + `pure` | `:<tag>-ol26` | 2.6, **Debian's own prebuilt binary**, pinned (`OPENLDAP_OL26_VERSION`). Not built by us at all — the most conservative thing on offer. Legacy interop and hot migration (ADR-011) |
| `lmdb0.9` + `slaptain` | `:<tag>-ol26src` | 2.6, Debian's **source** package rebuilt with our patches, same pin. Measurement arms against `-ol26`, and 2.6 clusters needing a patched feature |

The 2.6 version is pinned, not floated: one Makefile variable,
`OPENLDAP_OL26_VERSION`, drives both binary images and the source build, so
the pair cannot drift apart and the patches apply to a fixed tree. When a
Debian point release retires it the build stops resolving — bump the variable
and re-run the 2.6 e2e cycle (ADR-040 decision 1).

**The default pair is PURE since 0.5.0.** It used to carry the local patches;
it no longer does, because nobody should run a patched slapd without having
chosen to. The patches are reached by `flavour: slaptain` on either format
line, and the package revision in `slapd -VV` tells you which you got —
`+slaptain<N>` or `+pure<N>`. There is no image label for it; the version
string is the check, and before 0.5.0 it could not tell them apart at all.

`<tag>` is the same release tag for all three pairs — the suffixes are only
suffixes, never separate version lines. `slapd-toolkit` and `operator` have one tag each and
carry no suffix.

### Why 2.7 is the default

**Not** because of ITS#9580. This page used to say so; that reasoning is
retracted. The refresh storm we actually hit is
[ITS#9398](https://bugs.openldap.org/show_bug.cgi?id=9398), it is fixed in no
release of either line, and slaptain addresses it with a log-database
checkpoint that works on both (`docs/ITS-9398-HANDLING.md`).

The reason that holds is the format break itself. The two LMDB formats cannot
read each other, so **whichever line is the default, every cluster that said
nothing is eventually billed a dump-and-reload when that default moves.**
Defaulting to 2.7 — where the format line is going anyway — means whoever takes
the default never migrates, and anyone on 2.6 is there deliberately and can
plan the move. Defaulting to 2.6 would just mean changing our minds later, on
everyone else's maintenance window.

The cost of that choice is that **no distribution packages OpenLDAP 2.7 yet**.
It is released and supported upstream, but recent and not widely deployed, so
shipping it means maintaining a forward-ported Debian packaging fork
([`images/openldap-deb/`](../images/openldap-deb/README.md)). On the 2.6 line
`flavour: pure` gives you Debian's own binary instead; on 2.7 there is no such
option, and that asymmetry is real.

### `-ol26` and `-ol26src` are three characters apart and only one is patched

`-ol26` installs Debian's prebuilt binary, so it carries no local patch and has
nowhere to carry one. `-ol26src` compiles the same upstream version from
Debian's source with everything in
[`images/openldap-deb/local-patches/`](../images/openldap-deb/local-patches/)
applied.

The failure this invites is quiet: deploy `-ol26` expecting a patched feature,
and the configuration it needs is simply rejected, or worse accepted and
ignored — a clean-looking run in which the thing under test never happened.
`-ol26` is also the suffix a long-running 2.6 cluster already uses, so it is
the one a hand reaches for.

Two ways to tell them apart without deploying anything:

```bash
# the image says what it carries, without starting slapd
podman inspect --format '{{index .Config.Labels "net.chuck-chuck-chuck.slaptain.local-patches"}}'   <registry>/slaptain/slapd:<tag>-ol26src

# the binary says so too: a patched build carries a +slaptain… package version
podman run --rm --entrypoint="" <image> slapd -VV
```

Neither of the two dates in play answers the question, and they are different
dates with different causes — which is what makes them inviting to misread:

- the **image** creation date (`podman inspect --format '{{.Created}}'`) can
  sit well before the tag's commit, because a Containerfile that rarely changes
  means cached layers and an image that keeps its original date while taking a
  fresh tag;
- the **binary** build date, in `slapd -VV`, is older still for `-ol26` — it is
  *Debian's* build date for the prebuilt package, so it stays fixed no matter
  when the image was assembled. On `-ol26src` it is our own compile, so it
  tracks the build.

Measured on one pair: `-ol26` reads image 2026-10-06 and binary `May 29 2025`;
`-ol26src` reads image 2026-10-17 and binary `Oct 17 2026`. Judge by the label
and the version string, never by either date.

## If you point an image at the wrong format — read this before acting

Measured on a live cluster, 2026-10-23. A pod running the 2.6 image against
volumes written by 2.7 crashloops with:

```
mdb_db_open: database "cn=slaptain-auth" cannot be opened:
MDB_INVALID: File is not an LMDB file (-30793). Restore from backup!
backend_startup_one (type=mdb, suffix="cn=slaptain-auth"): bi_db_open failed! (-30793)
slapd stopped.
```

**Do not restore from backup.** Your data is fine. The message is LMDB's
generic "I cannot make sense of this file", and a volume written by the other
format reaches it before any version check — so it says `MDB_INVALID` rather
than a version complaint, and the advice attached to it is the advice for
corruption, which this is not. Restoring here would run the full scale-to-0
restore machine over healthy data.

Two more things that will confuse you if nobody has said them:

- **The database it names is `cn=slaptain-auth`**, which you never declared. It
  is the node-local authentication database (ADR-027) and simply happens to be
  the first one opened. The suffix in the message tells you nothing about which
  of *your* databases is affected — all of them are.
- **The init container succeeds.** It only writes configuration, so a green
  init container says nothing about whether slapd can open the volumes.

### Getting out

The roll stops at the first bad pod, so the rest of the fleet keeps serving —
you will see `Degraded`, `Ready=False NotReady: 2/3` and `PodsResponsive=False`
naming the stuck pod.

**Putting the spec back is necessary but not sufficient.** Correcting
`spec.images.select.storageFormat` (or the pinned tag) updates the StatefulSet
template, but the stuck pod stays on the old image: with `OrderedReady`,
Kubernetes will not replace a pod that is not Ready, so the broken pod blocks
its own replacement. Measured: template corrected, pod unchanged through five
restarts.

Delete it by hand once the spec is right:

```bash
kubectl delete pod <cluster>-<ordinal> -n <namespace>
```

It came back Ready in under 15 s with every database intact. Repeat per stuck
pod, lowest ordinal last.

## Pinning the 2.6 pair

Set both image tags on the `SlapdCluster` — **both, or neither**. The init
container writes the config and data volumes that slapd then opens, and 2.6 and
2.7 do not share an on-disk format.

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
spec:
  images:
    slapd:
      tag: <tag>-ol26
    init:
      tag: <tag>-ol26
```

With the `slapd` chart:

```bash
helm upgrade --install slapd ./charts/slapd \
  --set images.slapd.tag=<tag>-ol26 \
  --set images.init.tag=<tag>-ol26
```

Local build and delivery targets:

```bash
make build-ol26          # both -ol26 images
make import-ol26         # import them into the nodes' CRI
make push                # pushes all nine images (both 2.7 and both 2.6 pairs, the
                         # ol26src pair, toolkit, operator, exporter)
```

To run the e2e suite against the 2.6 pair (the operator image stays on the plain
tag):

```bash
SLAPD_TAG_SUFFIX=-ol26 ./tests/e2e.sh all <context>
```

`dataloss_recovery_test`'s no-storm assertion is the one to watch on a
multi-site run. It used to be described here as an ITS#9580 spec that 2.7
fixes; it is not. It asserts the absence of the ITS#9398 stale-cookie storm,
which affects both version lines and which no release fixes — what addresses it
is the accesslog `syncprov-checkpoint` that 0.5.0 turns on by default, on 2.6
and 2.7 alike. A fresh single-site cluster does not reproduce the storm at all
(no serverID is dormant), so a green single-site run says nothing either way.

## Migrating an existing 2.6 cluster to 2.7

> **Read this first.** 2.7 uses LMDB 1.0, whose on-disk format 2.6 slapd cannot
> read, and vice versa. Swapping the image tags on a running cluster is **not an
> upgrade**: slapd fails to open `/data` and `/accesslog`, and the pods
> crashloop. The migration is a dump and reload — back up on 2.6, switch the
> images, wipe the volumes, restore.
>
> Plan a window. The cluster is unavailable from step 3 until step 6 completes.

The runbook uses the backup/restore machinery from
[ADR-014](adrs/adr-014-s3-backup-restore.md) — see [`BACKUP.md`](BACKUP.md) for
the S3 credentials Secret and the `SlapdBackup` fields.

### 1. Back up, still on 2.6

```bash
kubectl apply -n <namespace> -f - <<'EOF'
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdBackup
metadata:
  name: pre-ol27
spec:
  databaseRef: <slapddatabase-name>
  storage:
    bucket: slaptain-backups
    endpoint: ""                       # empty for AWS S3; FQDN for an in-cluster store
    region: <region>
    credentialsSecretName: <s3-credentials-secret>
EOF

kubectl wait -n <namespace> slapdbackup/pre-ol27 \
  --for=jsonpath='{.status.phase}'=Completed --timeout=30m
```

Do not proceed until the phase is `Completed`. Note the object key from
`status`:

```bash
kubectl get -n <namespace> slapdbackup/pre-ol27 -o yaml
```

Take one backup per `SlapdDatabase` in the cluster.

### 2. Record the current state

```bash
kubectl get -n <namespace> slapdcluster/<cluster> \
  -o jsonpath='{.spec.images}{"\n"}{.spec.replicas}{"\t"}{.spec.readReplicas}{"\n"}'
kubectl get pvc -n <namespace>
```

Keep this. It is your rollback target: re-pinning `<tag>-ol26` and restoring the
same backup puts you back where you started.

This migration wipes the config and data volumes and reloads from a backup, so it
is also the one window in which you can change the tunables that are fixed once a
database or a `/config` volume exists — the map size, the LMDB environment flags
and the back-mdb IDL exponent. If any of them is wrong for your directory, set it
now rather than paying for a second wipe. See [Tuning & Sizing](TUNING.md).

### 3. Pin the 2.7 images

```bash
kubectl patch -n <namespace> slapdcluster/<cluster> --type merge -p \
  '{"spec":{"images":{"slapd":{"tag":"<tag>"},"init":{"tag":"<tag>"}}}}'
```

> **The crashloop window starts here.** The StatefulSet rolls each pod onto 2.7
> slapd, which cannot open the 2.6-format `/data` and `/accesslog` it finds, so
> the pods enter `CrashLoopBackOff` and stay there until step 4 gives them empty
> volumes. This is expected and is why the images are pinned *before* the
> volumes are wiped: doing it the other way round would have fresh volumes
> written by 2.6 and the break would simply move.
>
> `spec.replicas` cannot be set to 0 (the CRD requires ≥ 1), so there is no
> "scale down, swap, scale up" variant — crashlooping pods are how the cluster
> waits.

### 4. Wipe the volumes, per pod

PVCs from `volumeClaimTemplates` carry no labels, so delete them by name. A PVC
in use only gets a `deletionTimestamp` (the `kubernetes.io/pvc-protection`
finalizer); it is garbage-collected once no pod object references it, which
deleting the pod triggers. Any *other* pod naming the PVC — a finished backup
Job's pod included — blocks it too (ADR-018).

For each RW ordinal `i` in `0 … <replicas>-1`:

```bash
kubectl delete pvc -n <namespace> \
  config-<cluster>-$i data-<cluster>-$i accesslog-<cluster>-$i --wait=false
kubectl delete pod -n <namespace> <cluster>-$i
```

And for each read-only ordinal `j` (if `spec.readReplicas > 0` — these have no
accesslog volume):

```bash
kubectl delete pvc -n <namespace> \
  config-<cluster>-readonly-$j data-<cluster>-readonly-$j --wait=false
kubectl delete pod -n <namespace> <cluster>-readonly-$j
```

Confirm nothing is stuck before moving on — a PVC still in `Terminating` will
block its pod's volume mount:

```bash
kubectl get pvc -n <namespace>
kubectl get pods -n <namespace> -l app.kubernetes.io/instance=<cluster>
```

Fresh volumes are provisioned from the StatefulSet's `volumeClaimTemplates`, the
init container bootstraps `cn=config` on 2.7, and the operator recreates the
databases. The DIT comes back **empty**: the seed is a one-shot latch and does
not re-run (ADR-012). Step 6 is what puts the data back.

### 5. Confirm the pods are healthy and running 2.7

```bash
kubectl get pods -n <namespace> -l app.kubernetes.io/instance=<cluster> \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .spec.containers[?(@.name=="slapd")]}{.image}{end}{"\n"}{end}'

slctl status -n <namespace> <cluster>
slctl inspect -n <namespace> <cluster>
```

Every pod must be `Running` on the plain-tag image, with no `-ol26` left.

### 6. Restore the data

```bash
kubectl apply -n <namespace> -f - <<'EOF'
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdRestore
metadata:
  name: from-ol26
spec:
  databaseRef: <slapddatabase-name>
  source:
    backupRef: pre-ol27
EOF

kubectl wait -n <namespace> slapdrestore/from-ol26 \
  --for=jsonpath='{.status.phase}'=Completed --timeout=60m
```

The restore scales the cluster to 0, runs an offline `slapadd` on every pod and
scales back up — expect the cluster to be unavailable for its duration. One
restore at a time per cluster; repeat per database.

### 7. Verify

```bash
slctl inspect -n <namespace> <cluster>      # CSN convergence, topology, stanza counts
slctl ldapsearch -n <namespace> -b '<baseDN>' -s sub '(objectClass=*)' dn
```

Compare the entry count against what you had before step 3.

### Multi-site meshes

Migrate one site at a time. A 2.6 site and a 2.7 site replicate with each other
normally — syncrepl is unchanged across the versions. (A mixed mesh is
wire-compatible by construction; we have not run a cross-site validation of
one — see ADR-021.)

This page used to add that the un-migrated 2.6 sites "remain exposed to
ITS#9580". That is retracted: the storm we hit is ITS#9398, it is present in
both lines, and since 0.5.0 both are given the accesslog checkpoint that
addresses it. Migrating does not buy you an escape from it, and staying does
not expose you to it any more than 2.7 does.

### The alternative: wipe one pod and resync

You can also migrate a replicated cluster pod by pod: delete one pod's PVCs, let
it come back on the 2.7 images and refresh from its peers. It works — but it is
ADR-012 case 2, a pod rebuilt from replication, which is the exact shape that
reproduced the ITS#9398 stale-cookie storm six times before the accesslog
checkpoint default. That default is on both version lines now and two
three-site cycles have run clean, but this is a deliberate rebuild of every pod
in turn, so it is the operation most likely to find out whether that holds.
Prefer backup/restore for anything but a lab.

## Rollback to 2.6

Same procedure in reverse: back up on 2.7, delete the PVCs, pin `<tag>-ol26` on
both images, restore. There is no shortcut — the format break is symmetric.
