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


sub "A log DB ON DISK right after the very first writes (no kill yet)"
D0=$(disk_csns A); echo "$D0"
echo "disk minCSN sids:     $(sidset "$D0" minCSN)"
echo "disk contextCSN sids: $(sidset "$D0" contextCSN)"

say "2. SCENARIO=$SCENARIO"
case "$SCENARIO" in
  kill)   : ;;                                  # nothing but a kill -9 of A
  term)   : ;;                                  # nothing but a clean SIGTERM of A
  wipeB)  writer_start $PORT_A w; sleep 4
          stop B; rm -rf "$SRV/B/log" "$SRV/B/data"; mkdir -p "$SRV/B/log" "$SRV/B/data"
          start B $PORT_B; sleep 45; writer_stop; sleep 5 ;;
esac

say "3. served vs disk on A, immediately before stopping it"
SRVD=$(served_csns $PORT_A); sub served; echo "$SRVD"
DISK=$(disk_csns A);         sub disk;   echo "$DISK"
echo "served minCSN sids:     $(sidset "$SRVD" minCSN)"
echo "served contextCSN sids: $(sidset "$SRVD" contextCSN)"
echo "disk   minCSN sids:     $(sidset "$DISK" minCSN)"
echo "disk   contextCSN sids: $(sidset "$DISK" contextCSN)"

say "4. stop A ($([ "$SCENARIO" = term ] && echo SIGTERM || echo SIGKILL)) and restart"
if [ "$SCENARIO" = term ]; then stop A TERM; else stop A KILL; fi
sub "A log DB on disk while A is down"
D2=$(disk_csns A); echo "$D2"
echo "disk minCSN sids:     $(sidset "$D2" minCSN)"
echo "disk contextCSN sids: $(sidset "$D2" contextCSN)"
: > "$SRV/A.slapd.log"; : > "$SRV/B.slapd.log"
start A $PORT_A
sleep 20
sub "A served after restart"; served_csns $PORT_A
sub "storm rate on the provider A"
n0=$(stale_on A); sleep 10; n1=$(stale_on A)
echo "err=4096: $n0 -> $n1 => $(( (n1-n0)/10 ))/s"
grep -ao 'consumer not within recorded mincsn for DB.s mincsn=[^ ]*' "$SRV/A.slapd.log" | sort -u | head -3
sub "writes still replicate A->B?"
ldapadd -x -H ldap://127.0.0.1:$PORT_A -D "cn=admin,$SUFFIX" -w $PW >/dev/null 2>&1 <<EOF
dn: cn=after$SCENARIO,$SUFFIX
objectClass: organizationalRole
cn: after$SCENARIO
EOF
sleep 10
ldapsearch -LLL -x -H ldap://127.0.0.1:$PORT_B -b "cn=after$SCENARIO,$SUFFIX" dn 2>&1 | head -2

say "DONE scenario=$SCENARIO logckp='${LOGCKP}'"
writer_stop; stop A KILL; stop B KILL
