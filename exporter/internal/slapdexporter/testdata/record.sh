#!/bin/sh
# Records cn=Monitor from a throwaway slapd. Run inside the slapd-init image:
#   podman run --rm -v $PWD:/s:ro -v <out>:/out --entrypoint /s/record.sh <registry>/slapd-init:<tag>
# The output is the fixtures in this directory (each gets a provenance header).
set -eu
OUT=/out
W=/tmp/w; mkdir -p $W/slapd.d $W/db1 $W/db2 $W/run
SOCK=$W/run/slapd.ldapi
URI="ldapi://$(echo $SOCK | sed 's,/,%2f,g')"
ME="gidNumber=$(id -g)+uidNumber=$(id -u),cn=peercred,cn=external,cn=auth"
cat > $W/boot.ldif <<L
dn: cn=config
objectClass: olcGlobal
cn: config

dn: cn=module{0},cn=config
objectClass: olcModuleList
cn: module{0}
olcModulePath: /usr/lib/ldap
olcModuleLoad: back_mdb

dn: cn=schema,cn=config
objectClass: olcSchemaConfig
cn: schema

include: file:///etc/ldap/schema/core.ldif
include: file:///etc/ldap/schema/cosine.ldif

dn: olcDatabase={-1}frontend,cn=config
objectClass: olcDatabaseConfig
objectClass: olcFrontendConfig
olcDatabase: {-1}frontend

dn: olcDatabase={0}config,cn=config
objectClass: olcDatabaseConfig
olcDatabase: {0}config
olcRootDN: $ME

dn: olcDatabase={1}mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: {1}mdb
olcSuffix: dc=example,dc=org
olcDbDirectory: $W/db1
olcRootDN: $ME
olcAccess: to * by * read
L
slapadd -n0 -F $W/slapd.d -l $W/boot.ldif
slapd -F $W/slapd.d -h "$URI" -d 0 &
sleep 2
# monitor DB at runtime, AFTER dc=example,dc=org was opened: that DB gets no olmMDB*.
ldapadd -Q -Y EXTERNAL -H "$URI" <<L
dn: olcDatabase=monitor,cn=config
objectClass: olcDatabaseConfig
olcDatabase: monitor
olcRootDN: cn=admin,cn=config
olcAccess: to * by dn.children="cn=slaptain-auth" read by dn.children="cn=peercred,cn=external,cn=auth" read by * none
L
# an mdb ADDed AFTER the monitor DB: gets olmMDB*.
ldapadd -Q -Y EXTERNAL -H "$URI" <<L
dn: olcDatabase=mdb,cn=config
objectClass: olcDatabaseConfig
objectClass: olcMdbConfig
olcDatabase: mdb
olcSuffix: dc=example,dc=net
olcDbDirectory: $W/db2
olcRootDN: $ME
olcAccess: to * by * read
L
for s in org net; do
ldapadd -Q -Y EXTERNAL -H "$URI" <<L
dn: dc=example,dc=$s
objectClass: dcObject
objectClass: organization
dc: example
o: example

dn: ou=people,dc=example,dc=$s
objectClass: organizationalUnit
ou: people

dn: cn=alice,ou=people,dc=example,dc=$s
objectClass: person
cn: alice
sn: alice
L
done
# some traffic, so operation counters are not all zero
ldapadd -Q -Y EXTERNAL -H "$URI" -D x 2>/dev/null </dev/null || true
for s in org net; do
ldapsearch -LLL -x -H "$URI" -b "dc=example,dc=$s" -s base >/dev/null 2>&1 || true
done
ldapsearch -LLL -o ldif-wrap=no -Q -Y EXTERNAL -H "$URI" -b cn=Monitor -s sub '(objectClass=*)' '*' '+' > $OUT/monitor-full.ldif
ldapsearch -LLL -o ldif-wrap=no -Q -Y EXTERNAL -H "$URI" -b cn=Monitor -s sub '(|(objectClass=monitoredObject)(objectClass=monitorCounterObject)(objectClass=monitorOperation))' '*' '+' > $OUT/monitor-filtered.ldif
ldapsearch -LLL -o ldif-wrap=no -x -H "$URI" -b cn=Monitor -s base '(objectClass=*)' > $OUT/anon.txt 2>&1 || echo "anon rc=$?" >> $OUT/anon.txt
slapd -VVV > $OUT/version.txt 2>&1 || true
echo "$URI" > $OUT/uri.txt
