#!/bin/bash
#
# ITS#9580-adjacent: a permanent "sync cookie is stale" loop on OpenLDAP 2.7.1
# ===========================================================================
#
# Invariant under test:
#
#   On a delta-syncrepl provider, if the LOG DB's minCSN names a serverID that
#   the LOG DB's contextCSN does not contain, then every consumer of that
#   provider is told err=4096 "sync cookie is stale" on every connection,
#   forever, and retries with no backoff.
#
# This script builds the bad state deterministically (offline slapadd of a
# doctored log-DB dump -- no race, no replication history, no storage loss),
# demonstrates the symptom, shows that it does not self-heal, and then cures
# it by having the missing serverID write one entry.
#
# Two plain slapd processes, slapd.conf configuration, nothing else.
#
# Run:   ./its9580-repro.sh            (uses podman to supply slapd 2.7.1)
# On a VM with OpenLDAP 2.7.1 already installed: delete the CONTAINER PLUMBING
# block below and run the rest directly as root.
#
set -u

# ---------------------------------------------------------------- CONTAINER
# PLUMBING -- delete this block to run directly on a host that already has
# OpenLDAP 2.7.1 (slapd, slapadd, slapcat, ldapsearch, ldapadd) installed.
IMAGE=${IMAGE:-<registry>/slaptain/slapd-init:<tag>}
if [ "${ITS9580_INSIDE:-}" != 1 ]; then
    self=$(readlink -f "$0")
    exec podman run --rm -i --name its9580-repro --user 0 \
         --entrypoint /bin/bash -e ITS9580_INSIDE=1 \
         -v "$self":/repro.sh:ro,z "$IMAGE" /repro.sh "$@"
fi
# ------------------------------------------------------------ END PLUMBING

SCHEMA=${SCHEMA:-/etc/ldap/schema}
MODPATH=${MODPATH:-/usr/lib/ldap}
SRV=${SRV:-/srv/its9580}
SUFFIX="dc=example,dc=org"
LOGSUFFIX="cn=accesslog"
PW=secret
PORT_A=9001
PORT_B=9002
# slapd log level: stats(256) + sync(16384)
DBG=16640

say() { printf '\n\033[1m=== %s\033[0m\n' "$*"; }
sub() { printf '\n--- %s\n' "$*"; }

# ---------------------------------------------------------------- 0. config
write_conf() {   # $1 node  $2 serverID  $3 own port  $4 peer rid  $5 peer port
    local n=$1 sid=$2 port=$3 prid=$4 pport=$5; local d=$SRV/$n
    cat > "$d/slapd.conf" <<EOF
include $SCHEMA/core.schema
include $SCHEMA/cosine.schema
include $SCHEMA/inetorgperson.schema

pidfile  $d/slapd.pid
argsfile $d/slapd.args

modulepath $MODPATH
moduleload back_mdb
moduleload syncprov
moduleload accesslog

serverID $sid

# ---- change journal (the delta-syncrepl log DB) ------------------------
database  mdb
suffix    "$LOGSUFFIX"
directory $d/log
maxsize   1073741824
rootdn    "cn=admin,$LOGSUFFIX"
rootpw    $PW
index     entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN eq

overlay   syncprov
syncprov-nopresent  TRUE
syncprov-reloadhint TRUE

# ---- data --------------------------------------------------------------
database  mdb
suffix    "$SUFFIX"
directory $d/data
maxsize   1073741824
rootdn    "cn=admin,$SUFFIX"
rootpw    $PW
index     objectClass,entryCSN,entryUUID eq

multiprovider TRUE

overlay   syncprov

overlay   accesslog
logdb     "$LOGSUFFIX"
logops    writes
logsuccess TRUE
logpurge  07+00:00 01+00:00

syncrepl rid=$prid
  provider=ldap://127.0.0.1:$pport
  bindmethod=simple
  binddn="cn=admin,$SUFFIX"
  credentials=$PW
  searchbase="$SUFFIX"
  type=refreshAndPersist
  retry="5 +"
  syncdata=accesslog
  logbase="$LOGSUFFIX"
  logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"
EOF
}

start() {  # $1 node  $2 port
    local n=$1; local p=$2
    slapd -f "$SRV/$n/slapd.conf" -h "ldap://127.0.0.1:$p/" -d $DBG \
          >>"$SRV/$n.slapd.log" 2>&1 &
    for _ in $(seq 60); do
        ldapsearch -x -H "ldap://127.0.0.1:$p" -b "" -s base -LLL \
                   namingContexts >/dev/null 2>&1 && return 0
        sleep 0.5
    done
    echo "node $n did not come up" >&2; exit 1
}

stop() {   # $1 node
    local n=$1; local pf=$SRV/$n/slapd.pid
    [ -f "$pf" ] || return 0
    kill "$(cat "$pf")"
    for _ in $(seq 60); do [ -f "$pf" ] || return 0; sleep 0.5; done
    echo "node $n did not stop" >&2; exit 1
}

csns() {   # $1 port -- the log DB's minCSN and contextCSN
    ldapsearch -LLL -x -H "ldap://127.0.0.1:$1" -D "cn=admin,$LOGSUFFIX" -w $PW \
        -b "$LOGSUFFIX" -s base minCSN contextCSN
}

stale_count() { grep -ac 'err=4096' "$SRV/B.slapd.log" 2>/dev/null || echo 0; }

rm -rf "$SRV"; mkdir -p "$SRV"/A/log "$SRV"/A/data "$SRV"/B/log "$SRV"/B/data
write_conf A 1 $PORT_A 002 $PORT_B
write_conf B 2 $PORT_B 001 $PORT_A

say "slapd build under test"
slapd -VV 2>&1

say "configuration (identical on both nodes bar serverID/ports/paths)"
cat "$SRV/A/slapd.conf"

# ------------------------------------------------- 1. a healthy 2-node mesh
say "1. seed the suffix on A, start both nodes"
cat > "$SRV/base.ldif" <<EOF
dn: $SUFFIX
objectClass: top
objectClass: dcObject
objectClass: organization
o: Example
dc: example
EOF
slapadd -q -f "$SRV/A/slapd.conf" -b "$SUFFIX" -l "$SRV/base.ldif"
start A $PORT_A
start B $PORT_B

say "2. write at A and at B so both serverIDs enter the journals"
ldapadd -x -H ldap://127.0.0.1:$PORT_A -D "cn=admin,$SUFFIX" -w $PW <<EOF
dn: cn=fromA,$SUFFIX
objectClass: organizationalRole
cn: fromA
EOF
ldapadd -x -H ldap://127.0.0.1:$PORT_B -D "cn=admin,$SUFFIX" -w $PW <<EOF
dn: cn=fromB,$SUFFIX
objectClass: organizationalRole
cn: fromB
EOF
sleep 5
sub "A data"; ldapsearch -LLL -x -H ldap://127.0.0.1:$PORT_A -b "$SUFFIX" dn
sub "B data"; ldapsearch -LLL -x -H ldap://127.0.0.1:$PORT_B -b "$SUFFIX" dn
sub "A log DB"; csns $PORT_A
sub "B log DB"; csns $PORT_B
echo
echo "Both nodes healthy: minCSN's SID set is contained in contextCSN's."
echo "Stale results so far: $(stale_count)"

# ------------------------------------------- 3. manufacture the divergence
say "3. stop A, remove ONE contextCSN value from its log DB, reload offline"
stop A
stop B
slapcat -o ldif-wrap=no -f "$SRV/A/slapd.conf" -b "$LOGSUFFIX" -l "$SRV/A-log.ldif"

sub "A's log DB suffix entry as dumped"
sed -n '1,/^$/p' "$SRV/A-log.ldif"

# Drop the contextCSN whose SID is 002. minCSN keeps it. This is exactly the
# end state the accesslog FIXME at accesslog.c:2069 describes: a destructive
# MOD REPLACE of contextCSN copied onto the log DB removes SIDs, minCSN never
# shrinks.
VICTIM_SID=002
grep -v "^contextCSN: .*#${VICTIM_SID}#" "$SRV/A-log.ldif" > "$SRV/A-log-bad.ldif"

PREDICTED=$(grep "^minCSN: " "$SRV/A-log-bad.ldif" | sed 's/^minCSN: //' | \
            awk -F'#' '{print $3, $0}' | sort | \
            while read -r sid rest; do
                grep -q "^contextCSN: .*#$sid#" "$SRV/A-log-bad.ldif" || { echo "$rest"; break; }
            done)

sub "A's log DB suffix entry after the edit"
sed -n '1,/^$/p' "$SRV/A-log-bad.ldif"

rm -rf "$SRV/A/log"; mkdir -p "$SRV/A/log"
slapadd -q -f "$SRV/A/slapd.conf" -b "$LOGSUFFIX" -l "$SRV/A-log-bad.ldif"
echo "slapadd accepted the NO-USER-MODIFICATION minCSN verbatim."

say "PREDICTION, made before starting anything"
cat <<EOF
  syncprov.c:3480 walks minCSN in SID order and fails on the first SID that
  contextCSN does not carry.  The lowest minCSN SID absent from contextCSN is
  $VICTIM_SID, so A must log exactly:

      syncprov_op_search: consumer not within recorded mincsn for DB's
      mincsn=$PREDICTED

  and must answer EVERY consumer cookie with err=4096 "sync cookie is stale".
EOF

# ------------------------------------------------------------- 4. the storm
say "4. start A (bad state) then B (healthy consumer)"
: > "$SRV/A.slapd.log"; : > "$SRV/B.slapd.log"
start A $PORT_A
start B $PORT_B

sleep 30
sub "A's log DB, live"
csns $PORT_A

sub "provider side: distinct 'not within recorded mincsn' messages on A"
grep -ao 'consumer not within recorded mincsn for DB.s mincsn=[^ ]*' \
     "$SRV/A.slapd.log" | sort -u

sub "provider side: one stale result in full"
grep -a -m1 -B2 'sync cookie is stale' "$SRV/A.slapd.log"

sub "consumer side: B's reaction"
grep -a -m4 -E 'lost sync|REFRESH|rid=001.*(stale|refresh)' "$SRV/B.slapd.log"

sub "rate, measured over 10 s on the consumer"
n0=$(stale_count); sleep 10; n1=$(stale_count)
echo "err=4096 results seen by B: $n0 -> $n1  => $(( (n1-n0) / 10 )) per second"
echo "(retry=\"5 +\" is configured and is never consulted on this path)"

sub "does it heal?  sampling every 30 s"
for i in 1 2 3; do sleep 30; printf '  t+%3ds  cumulative err=4096 on B: %s\n' $((40+i*30)) "$(stale_count)"; done

# --------------------------------------------------------------- 5. the cure
say "5. the cure: have the missing serverID write one entry"
echo "serverID $VICTIM_SID is node B.  One write at B re-enters SID $VICTIM_SID in"
echo "A's log DB contextCSN via syncprov_new_ctxcsn().  minCSN is not touched."
before=$(stale_count)
ldapadd -x -H ldap://127.0.0.1:$PORT_B -D "cn=admin,$SUFFIX" -w $PW <<EOF
dn: cn=cure,$SUFFIX
objectClass: organizationalRole
cn: cure
EOF
sleep 30
sub "A's log DB after the cure write"
csns $PORT_A
mid=$(stale_count); sleep 10; after=$(stale_count)
sub "rate after the cure"
echo "err=4096 on B: $mid -> $after  => $(( (after-mid) / 10 )) per second"
echo "(was $(( (n1-n0) / 10 )) per second before; cumulative before cure: $before)"

sub "replication still works"
ldapsearch -LLL -x -H ldap://127.0.0.1:$PORT_A -b "$SUFFIX" dn

say "done"
stop A; stop B
