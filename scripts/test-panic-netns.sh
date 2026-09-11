#!/bin/bash
# test-panic-netns.sh: prove ouf-panic can revert host-network changes,
# executed inside a throwaway user+mount+net namespace. Uses env
# overrides (OUF_STATE_DIR, OUF_RESOLV_PATH) so that /etc/resolv.conf on
# the host is NEVER touched, even if isolation guarantees fail.
#
# Requires: sudo, ip, unshare, jq.
#
# Test matrix:
#   1. no snapshot                                   -> exit 0
#   2. only tun_create                               -> tun deleted, snapshot cleared
#   3. full state, resolv.conf was a regular file    -> everything reverted
#   4. resolv.conf was a symlink                     -> symlink restored
#   5. bogus tun_ifname=eth0                          -> refused, exit 2
#   6. bogus table_id=254                             -> refused, exit 2
#   7. tun already absent (idempotence)              -> exit 0
#   8. panic run twice                                -> both runs succeed

set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PANIC="$SCRIPT_DIR/ouf-panic"
[ -x "$PANIC" ] || { echo "FATAL: $PANIC not executable" >&2; exit 2; }

cat > /tmp/ouf-panic-test-inner.sh <<'INNER_EOF'
#!/bin/bash
set -u
PANIC="$1"
CASE_NAME="$2"
CASE_KIND="$3"

PASS=0
FAIL=0

STATE_DIR="$(mktemp -d)"
FAKE_RESOLV="$STATE_DIR/fake-resolv.conf"

# Every case gets these:
export OUF_STATE_DIR="$STATE_DIR"
export OUF_RESOLV_PATH="$FAKE_RESOLV"
export OUF_VERBOSE=1

mkdir -p "$STATE_DIR/backup"
# Bring loopback up
ip link set lo up

pass() { echo "  PASS: $*"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
check() { if [ "$1" = "1" ]; then pass "$2"; else fail "$2"; fi; }

# Guard: refuse to run if the paths escaped somehow
case "$FAKE_RESOLV" in
    /tmp/*|/var/tmp/*) ;;
    *) echo "FATAL: fake resolv path not in tmp: $FAKE_RESOLV" >&2; exit 99 ;;
esac

case "$CASE_KIND" in

no_snapshot)
    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "0" ] && check 1 "exit 0" || check 0 "exit=$rc"
    [ ! -e "$STATE_DIR/snapshot.json" ] && check 1 "no snapshot created" || check 0 "snapshot appeared"
    ;;

tun_only)
    ip tuntap add ouftest0 mode tun
    ip link set ouftest0 up
    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftest0","table_id":9999,"rule_priority":29999,"fwmark":null},
 "pre_state":{"resolv_conf":{"existed":false,"backup_path":"","was_symlink":false,"symlink_target":"","sha256_before":""},"tun_existed":false,"sysctls":[]},
 "actions":[{"seq":1,"op":"tun_create","ifname":"ouftest0","done":true}]}
JSON
    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "0" ] && check 1 "exit 0" || { check 0 "exit=$rc"; cat /tmp/err; }
    ip link show ouftest0 >/dev/null 2>&1 && check 0 "tun still present" || check 1 "tun deleted"
    [ ! -e "$STATE_DIR/snapshot.json" ] && check 1 "snapshot cleared" || check 0 "snapshot not cleared"
    ;;

full_state_regular_resolv)
    ip tuntap add ouftest0 mode tun
    ip link set ouftest0 up
    ip route add 10.99.99.0/24 dev ouftest0 table 9999
    ip rule add priority 29999 table 9999

    # Original (pre-connect) resolv.conf: put content in the backup slot
    ORIG_CONTENT="nameserver 192.168.0.1"
    echo "$ORIG_CONTENT" > "$STATE_DIR/backup/resolv.conf"
    SHA=$(sha256sum "$STATE_DIR/backup/resolv.conf" | awk '{print $1}')

    # Current (mutated) resolv.conf: simulate what the daemon would have written
    echo "nameserver 9.9.9.9" > "$FAKE_RESOLV"

    # Sysctl in the ns: change lo.disable_ipv6
    BEFORE=$(sysctl -n net.ipv6.conf.lo.disable_ipv6 2>/dev/null || echo 0)
    sysctl -w -q net.ipv6.conf.lo.disable_ipv6=1 || true

    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftest0","table_id":9999,"rule_priority":29999,"fwmark":null},
 "pre_state":{
   "resolv_conf":{"existed":true,"backup_path":"$STATE_DIR/backup/resolv.conf","was_symlink":false,"symlink_target":"","sha256_before":"$SHA"},
   "tun_existed":false,
   "sysctls":[{"key":"net.ipv6.conf.lo.disable_ipv6","value_before":"$BEFORE"}]
 },
 "actions":[
   {"seq":1,"op":"tun_create","ifname":"ouftest0","done":true},
   {"seq":2,"op":"route_add","table":9999,"spec":"10.99.99.0/24 dev ouftest0","done":true},
   {"seq":3,"op":"rule_add","priority":29999,"table":9999,"spec":"","done":true},
   {"seq":4,"op":"sysctl_set","key":"net.ipv6.conf.lo.disable_ipv6","value":"1","done":true}
 ]}
JSON

    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "0" ] && check 1 "exit 0" || { check 0 "exit=$rc"; cat /tmp/err; }
    ip link show ouftest0 >/dev/null 2>&1 && check 0 "tun still present" || check 1 "tun deleted"
    ip rule show priority 29999 2>/dev/null | grep -q . && check 0 "rule still present" || check 1 "rule deleted"
    [ -z "$(ip route show table 9999 2>/dev/null)" ] && check 1 "table 9999 flushed" || check 0 "table 9999 still has routes"
    NOW=$(cat "$FAKE_RESOLV")
    [ "$NOW" = "$ORIG_CONTENT" ] && check 1 "resolv.conf restored" || check 0 "resolv.conf is: $NOW"
    AFTER=$(sysctl -n net.ipv6.conf.lo.disable_ipv6 2>/dev/null || echo unknown)
    [ "$AFTER" = "$BEFORE" ] && check 1 "sysctl restored ($AFTER)" || check 0 "sysctl not restored (got $AFTER, expected $BEFORE)"
    [ ! -e "$STATE_DIR/snapshot.json" ] && check 1 "snapshot cleared" || check 0 "snapshot not cleared"
    ;;

resolv_symlink)
    # Fake stub target (what /etc/resolv.conf's symlink pointed at)
    STUB="$STATE_DIR/stub-resolv.conf"
    echo "nameserver 127.0.0.53" > "$STUB"
    # Also record it as the "backup" (for a symlink, the backup path is the same target)
    cp -a "$STUB" "$STATE_DIR/backup/resolv.conf"

    # Simulate: connect replaced the symlink with a regular file
    echo "nameserver 9.9.9.9" > "$FAKE_RESOLV"

    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftest0-nonexistent","table_id":9998,"rule_priority":29998,"fwmark":null},
 "pre_state":{
   "resolv_conf":{"existed":true,"backup_path":"$STATE_DIR/backup/resolv.conf","was_symlink":true,"symlink_target":"$STUB","sha256_before":""},
   "tun_existed":false,"sysctls":[]},
 "actions":[{"seq":1,"op":"resolv_write","path":"$FAKE_RESOLV","sha256_after":"","done":true}]}
JSON

    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "0" ] && check 1 "exit 0" || { check 0 "exit=$rc"; cat /tmp/err; }
    if [ -L "$FAKE_RESOLV" ]; then
        TGT=$(readlink "$FAKE_RESOLV")
        [ "$TGT" = "$STUB" ] && check 1 "symlink restored -> $TGT" || check 0 "symlink target wrong: $TGT"
    else
        check 0 "resolv path is not a symlink after revert"
    fi
    ;;

bogus_tun_eth0)
    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"eth0","table_id":9999,"rule_priority":29999,"fwmark":null},"pre_state":{"resolv_conf":{"existed":false,"backup_path":"","was_symlink":false,"symlink_target":"","sha256_before":""},"tun_existed":false,"sysctls":[]},"actions":[]}
JSON
    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "2" ] && check 1 "refused with exit 2" || check 0 "exit=$rc, expected 2"
    grep -q "refusing to touch reserved" /tmp/err && check 1 "correct refusal message" || check 0 "no refusal message"
    ;;

bogus_table_id)
    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftestx","table_id":254,"rule_priority":29999,"fwmark":null},"pre_state":{"resolv_conf":{"existed":false,"backup_path":"","was_symlink":false,"symlink_target":"","sha256_before":""},"tun_existed":false,"sysctls":[]},"actions":[]}
JSON
    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "2" ] && check 1 "refused with exit 2" || check 0 "exit=$rc, expected 2"
    ;;

tun_absent)
    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftestgh","table_id":9998,"rule_priority":29998,"fwmark":null},"pre_state":{"resolv_conf":{"existed":false,"backup_path":"","was_symlink":false,"symlink_target":"","sha256_before":""},"tun_existed":false,"sysctls":[]},"actions":[{"seq":1,"op":"tun_create","ifname":"ouftestgh","done":true}]}
JSON
    "$PANIC" >/tmp/out 2>/tmp/err
    rc=$?
    [ "$rc" = "0" ] && check 1 "exit 0 when tun absent" || check 0 "exit=$rc"
    [ ! -e "$STATE_DIR/snapshot.json" ] && check 1 "snapshot cleared" || check 0 "snapshot not cleared"
    ;;

twice)
    ip tuntap add ouftesttw mode tun
    cat > "$STATE_DIR/snapshot.json" <<JSON
{"version":1,"our_marks":{"tun_ifname":"ouftesttw","table_id":9997,"rule_priority":29997,"fwmark":null},"pre_state":{"resolv_conf":{"existed":false,"backup_path":"","was_symlink":false,"symlink_target":"","sha256_before":""},"tun_existed":false,"sysctls":[]},"actions":[{"seq":1,"op":"tun_create","ifname":"ouftesttw","done":true}]}
JSON
    "$PANIC" >/tmp/out 2>/tmp/err
    rc1=$?
    [ "$rc1" = "0" ] && check 1 "first run exit 0" || check 0 "first exit=$rc1"
    "$PANIC" >/tmp/out 2>/tmp/err
    rc2=$?
    [ "$rc2" = "0" ] && check 1 "second run exit 0 (idempotent)" || check 0 "second exit=$rc2"
    ;;
esac

echo "  RESULT: $PASS pass, $FAIL fail"
exit $FAIL
INNER_EOF
chmod +x /tmp/ouf-panic-test-inner.sh

CASES=(
    "no snapshot present::no_snapshot"
    "only tun_create::tun_only"
    "full state, regular resolv.conf::full_state_regular_resolv"
    "resolv.conf was a symlink::resolv_symlink"
    "bogus tun_ifname=eth0 refused::bogus_tun_eth0"
    "bogus table_id=254 refused::bogus_table_id"
    "tun already absent (idempotence)::tun_absent"
    "panic run twice::twice"
)

TOTAL_FAIL=0
for entry in "${CASES[@]}"; do
    NAME="${entry%%::*}"
    KIND="${entry##*::}"
    echo "=== case: $NAME ==="
    # Rootless: -Umnr maps our uid to root inside the new userns, so
    # we get CAP_NET_ADMIN et al. inside the netns without touching the
    # host and without needing sudo.
    if ! unshare -Umnr --propagation=private -- \
            /tmp/ouf-panic-test-inner.sh "$PANIC" "$NAME" "$KIND"; then
        TOTAL_FAIL=$((TOTAL_FAIL+1))
    fi
done

echo
if [ "$TOTAL_FAIL" = "0" ]; then
    echo "ALL CASES PASSED"
    rm -f /tmp/ouf-panic-test-inner.sh
    exit 0
else
    echo "$TOTAL_FAIL CASE(S) FAILED"
    exit 1
fi
