#!/bin/bash
# Part A: reach the ITS#9398 bad state using ONLY supported operations.
#   write entries | stop a node | delete a node's database directories | start a node
# No LDIF editing, no slapadd of doctored data, nothing touching minCSN/contextCSN.
set -u

SCHEMA=/etc/ldap/schema
MODPATH=/usr/lib/ldap
SRV=/srv/exp
SCENARIO=${SCENARIO:-kill}
SUFFIX="dc=example,dc=org"
LOGSUFFIX="cn=accesslog"
PW=secret
PORT_A=9001
PORT_B=9002
DBG=16640
LOGCKP=${LOGCKP:-}        # Part B: value for syncprov-checkpoint on the LOG DB

say() { printf '\n\033[1m=== %s\033[0m\n' "$*"; }
sub() { printf '\n--- %s\n' "$*"; }

write_conf() {   # $1 node  $2 serverID  $3 own port  $4 peer rid  $5 peer port
    local n=$1; local sid=$2; local port=$3; local prid=$4; local pport=$5; local d=$SRV/$n
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
${LOGCKP:+syncprov-checkpoint $LOGCKP}

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
    for _ in $(seq 120); do
        ldapsearch -x -H "ldap://127.0.0.1:$p" -b "" -s base -LLL \
                   namingContexts >/dev/null 2>&1 && return 0
        sleep 0.5
    done
    echo "node $n did not come up" >&2; exit 1
}

stop() {   # $1 node  [$2 signal, default TERM]
    local n=$1; local sig=${2:-TERM}; local pf=$SRV/$n/slapd.pid
    [ -f "$pf" ] || return 0
    local pid; pid=$(cat "$pf")
    kill -$sig "$pid"
    for _ in $(seq 120); do kill -0 "$pid" 2>/dev/null || { rm -f "$pf"; return 0; }; sleep 0.5; done
    echo "node $n did not stop" >&2; exit 1
}

served_csns() { # $1 port -- what the running slapd answers for the LOG DB
    ldapsearch -LLL -x -H "ldap://127.0.0.1:$1" -D "cn=admin,$LOGSUFFIX" -w $PW \
        -b "$LOGSUFFIX" -s base minCSN contextCSN 2>&1
}

disk_csns() {   # $1 node -- what is ON DISK in the LOG DB right now (live copy)
    local n=$1; local d=$SRV/$n
    rm -rf "$SRV/diskread"; mkdir -p "$SRV/diskread/log"
    cp -a "$d"/log/* "$SRV/diskread/log/" 2>/dev/null
    sed -e "s#directory $d/log#directory $SRV/diskread/log#" \
        -e "s#pidfile .*#pidfile $SRV/diskread/pid#" \
        -e "s#argsfile .*#argsfile $SRV/diskread/args#" \
        "$d/slapd.conf" > "$SRV/diskread/slapd.conf"
    slapcat -o ldif-wrap=no -f "$SRV/diskread/slapd.conf" -b "$LOGSUFFIX" 2>/dev/null \
        | sed -n '1,/^$/p' | grep -E '^(dn|minCSN|contextCSN):'
}

sidset() { grep "^$2: " <<<"$1" | sed 's/.*#\([0-9a-f]*\)#.*/\1/' | sort -u | tr '\n' ' '; }

writer_start() { # $1 port  $2 tag
    ( i=0; while :; do i=$((i+1))
        ldapadd -x -H "ldap://127.0.0.1:$1" -D "cn=admin,$SUFFIX" -w $PW >/dev/null 2>&1 <<EOF
dn: cn=$2$i,$SUFFIX
objectClass: organizationalRole
cn: $2$i
EOF
        sleep 0.2
      done ) &
    echo $! > "$SRV/writer.pid"
}
writer_stop() { [ -f "$SRV/writer.pid" ] && { kill "$(cat "$SRV/writer.pid")" 2>/dev/null; pkill -P "$(cat "$SRV/writer.pid")" 2>/dev/null; rm -f "$SRV/writer.pid"; }; sleep 1; return 0; }

stale_on() { grep -ac 'err=4096' "$SRV/$1.slapd.log" 2>/dev/null; true; }

# =========================================================================
rm -rf "$SRV"; mkdir -p "$SRV"/A/log "$SRV"/A/data "$SRV"/B/log "$SRV"/B/data
write_conf A 1 $PORT_A 002 $PORT_B
write_conf B 2 $PORT_B 001 $PORT_A

say "build under test"; slapd -VV 2>&1
say "config (A); LOGCKP='${LOGCKP}'"; grep -n 'syncprov-checkpoint\|^database\|^suffix\|serverID' "$SRV/A/slapd.conf"

say "1. seed A, start both, write from BOTH sids, converge"
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
for h in $PORT_A $PORT_B; do
  ldapadd -x -H ldap://127.0.0.1:$h -D "cn=admin,$SUFFIX" -w $PW >/dev/null <<EOF
dn: cn=seed$h,$SUFFIX
objectClass: organizationalRole
cn: seed$h
EOF
done
sleep 8
sub "A log DB (served)"; A1=$(served_csns $PORT_A); echo "$A1"
sub "B log DB (served)"; served_csns $PORT_B
echo
echo "A minCSN sids:     $(sidset "$A1" minCSN)"
echo "A contextCSN sids: $(sidset "$A1" contextCSN)"



say "2. THE MEASUREMENT: A's log-DB CSNs, served vs on disk -- A is still running,"
say "   has never been restarted, and nothing unusual has happened to it."
sub "SERVED  (ldapsearch against the live slapd -- syncprov's in-memory si_ctxcsn)"
SRVD=$(served_csns $PORT_A); echo "$SRVD"
sub "ON DISK (slapcat of a byte copy of the live LMDB directory)"
DISK=$(disk_csns A); echo "$DISK"
sub "verdict"
echo "served minCSN sids:     $(sidset "$SRVD" minCSN)"
echo "served contextCSN sids: $(sidset "$SRVD" contextCSN)"
echo "disk   minCSN sids:     $(sidset "$DISK" minCSN)"
echo "disk   contextCSN sids: $(sidset "$DISK" contextCSN)"
cat <<'TXT'

  The log DB's minCSN is written to disk eagerly (accesslog_response does a real
  MOD_ADD for every new SID).  The log DB's contextCSN is only ever written to
  disk by syncprov_checkpoint -- which no checkpoint is configured to call, so
  the only remaining caller is syncprov_db_close at a CLEAN shutdown.
  So the on-disk invariant is already violated, and has been since the first write.
  It is harmless while the process lives, because searches use si_ctxcsn.

TXT

say "3. node A crash-stops (SIGKILL: node loss / OOM / lost power / grace-period expiry)"
stop A KILL
sub "A's log DB on disk while A is down -- this is what the next startup will load"
D2=$(disk_csns A); echo "$D2"
echo "disk minCSN sids:     $(sidset "$D2" minCSN)   <- loaded by accesslog_db_open"
echo "disk contextCSN sids: $(sidset "$D2" contextCSN)   <- loaded by syncprov_db_open"

say "PREDICTION, before A is started again"
echo "  minCSN names SID 002, contextCSN does not.  syncprov.c:3480 must therefore"
echo "  answer EVERY consumer cookie with err=4096 'sync cookie is stale', naming"
echo "  the 002 minCSN value, and syncrepl.c:2336/:2383 must retry with no backoff."

say "4. start A again"
: > "$SRV/A.slapd.log"; : > "$SRV/B.slapd.log"
start A $PORT_A
sleep 20
sub "A served after restart"; served_csns $PORT_A
sub "provider A: distinct mincsn complaints"
grep -ao 'consumer not within recorded mincsn for DB.s mincsn=[^ ]*' "$SRV/A.slapd.log" | sort -u
sub "provider A: one stale result in full"
grep -a -m1 -B2 'sync cookie is stale' "$SRV/A.slapd.log"
sub "rate, measured over 10 s"
n0=$(stale_on A); sleep 10; n1=$(stale_on A)
echo "err=4096 on A: $n0 -> $n1  => $(( (n1-n0)/10 )) per second   (retry=\"5 +\" never consulted)"
sub "does it heal?"
for i in 1 2; do sleep 30; printf '  t+%3ds  cumulative err=4096 on A: %s\n' $((30+i*30)) "$(stale_on A)"; done

say "5. cure: one write from the ABSENT serverID (B, sid 002)"
before=$(stale_on A)
ldapadd -x -H ldap://127.0.0.1:$PORT_B -D "cn=admin,$SUFFIX" -w $PW >/dev/null <<EOF
dn: cn=cure,$SUFFIX
objectClass: organizationalRole
cn: cure
EOF
sleep 20
sub "A's log DB served, after the cure write"; served_csns $PORT_A
m0=$(stale_on A); sleep 10; m1=$(stale_on A)
echo "err=4096 on A: $m0 -> $m1  => $(( (m1-m0)/10 )) per second  (was $(( (n1-n0)/10 ))/s)"

say "DONE"
stop A KILL; stop B KILL
