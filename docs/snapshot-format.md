# Snapshot format

The **snapshot** is a JSON file the daemon writes to disk *before* mutating any host network state. `ouf-panic` reads it back to revert. Format is the source of truth for the revert; the daemon does not need to be alive for revert to work.

## Location

`/var/lib/outline-fedora/snapshot.json` (mode 0600, root:root)

Backups of touched files are stored alongside:
- `/var/lib/outline-fedora/backup/resolv.conf`
- `/var/lib/outline-fedora/backup/<other-file>`

The whole `/var/lib/outline-fedora/` directory is the panic script's scratch space. If it's gone, there's nothing to revert.

## Lifecycle

1. Daemon receives `connect` request.
2. Daemon builds snapshot in memory by reading current host state.
3. Daemon writes snapshot atomically (`snapshot.json.tmp` + rename) and fsyncs.
4. Only THEN does the daemon start mutating host state.
5. On clean `disconnect`, daemon runs revert, then deletes `snapshot.json`.
6. On any abnormal exit (crash, SIGKILL, panic), `snapshot.json` remains. `ouf-panic` (invoked by `ExecStopPost=` or manually) reads it and reverts.

**Invariant**: if `snapshot.json` exists, the machine may be in a mutated state and `ouf-panic` should be run.

## Schema (version 1)

```json
{
  "version": 1,
  "started_at": "2026-09-11T13:00:00Z",
  "our_marks": {
    "tun_ifname": "outline-tun0",
    "table_id": 233,
    "rule_priority": 23333,
    "fwmark": null
  },
  "created_by": {
    "hostname": "psl",
    "pid": 12345,
    "ouf_version": "0.1.0"
  },
  "pre_state": {
    "resolv_conf": {
      "existed": true,
      "backup_path": "/var/lib/outline-fedora/backup/resolv.conf",
      "was_symlink": true,
      "symlink_target": "../run/systemd/resolve/stub-resolv.conf",
      "sha256_before": "abc123..."
    },
    "tun_existed": false,
    "sysctls": [
      {"key": "net.ipv6.conf.all.disable_ipv6", "value_before": "0"}
    ]
  },
  "actions": [
    {"seq": 1, "op": "tun_create",   "ifname": "outline-tun0", "done": true},
    {"seq": 2, "op": "route_add",    "table": 233, "spec": "default dev outline-tun0", "done": true},
    {"seq": 3, "op": "rule_add",     "priority": 23333, "table": 233, "spec": "not from all fwmark 0x1", "done": true},
    {"seq": 4, "op": "sysctl_set",   "key": "net.ipv6.conf.all.disable_ipv6", "value": "1", "done": true},
    {"seq": 5, "op": "resolv_write", "path": "/etc/resolv.conf", "sha256_after": "def456...", "done": true}
  ]
}
```

### Field semantics

**`our_marks`** — identifies "things we own." The panic script uses these to know exactly what to delete. If `outline-tun0` doesn't exist, that step is a no-op (idempotent). If some other process has created a `outline-tun0` in the meantime, that's a caller error — we still delete it, because the invariant is "we created it."

**`created_by.pid`** — informational only. Panic never checks whether that pid is alive; it always reverts if the snapshot exists.

**`pre_state.resolv_conf.was_symlink` + `symlink_target`** — critical on Fedora. `/etc/resolv.conf` is usually a symlink to `/run/systemd/resolve/stub-resolv.conf`. On revert, we must restore the symlink (with correct target), not a regular file with symlink content. If it was a regular file, we restore it as a regular file with the backed-up content.

**`pre_state.resolv_conf.sha256_before`** — used by the panic script to sanity-check the backup: if the backup doesn't hash to the recorded value, refuse to restore (backup was tampered).

**`pre_state.resolv_conf.existed: false`** — if `/etc/resolv.conf` did not exist before, revert deletes it (rather than writing an empty file).

**`actions[].done`** — false means "we tried but failed" or "we wrote the intent to do this but crashed before starting." Revert treats `done=false` the same as `done=true` (delete/undo anyway — deletes of nonexistent things are no-ops).

**`actions[].op`** — one of a closed enum. New op types require a new `version:`.

### Operation types and their reverts

| op | forward | revert |
|---|---|---|
| `tun_create` | `ip tuntap add $ifname mode tun; ip link set $ifname up` | `ip link delete $ifname` (idempotent) |
| `route_add` | `ip route add $spec table $table` | `ip route flush table $table` (nuclear on our table — safe because it's our table) |
| `rule_add` | `ip rule add priority $priority table $table $spec` | `ip rule del priority $priority` (idempotent — `|| true`) |
| `sysctl_set` | `sysctl -w $key=$value` | `sysctl -w $key=$value_before` from `pre_state.sysctls` |
| `resolv_write` | write new `/etc/resolv.conf` | restore from `pre_state.resolv_conf` (see below) |

### Resolv.conf revert algorithm

```
if snapshot.pre_state.resolv_conf.existed == false:
    unlink /etc/resolv.conf if it exists
    return

verify sha256(backup_path) == pre_state.resolv_conf.sha256_before
    (if mismatch, log loud warning, DO NOT restore, exit nonzero)

unlink /etc/resolv.conf

if pre_state.resolv_conf.was_symlink:
    ln -s $symlink_target /etc/resolv.conf
else:
    cp -a backup_path /etc/resolv.conf
```

## What the panic script will NOT do (out of scope)

- Never restart NetworkManager, systemd-networkd, systemd-resolved. If they were fine before, they're still fine.
- Never `iptables -F` or touch nftables rules we didn't add.
- Never modify persistent config (`/etc/NetworkManager/`, `/etc/systemd/network/`, `/etc/systemd/resolved.conf`).
- Never touch a TUN device with a name other than `our_marks.tun_ifname`.
- Never touch a routing table with an ID other than `our_marks.table_id`.
- Never touch a rule at a priority other than `our_marks.rule_priority`.

The one thing that is nuclear-but-safe: `ip route flush table 233` removes ALL routes from table 233. That's fine because table 233 is ours — nothing else on the system uses a table ID that high unless the admin picked it (in which case ID collision is their config problem, and we can make the table ID configurable later).

## Idempotence

Running `ouf-panic` twice, or with a snapshot that describes actions we never actually got around to performing, must be safe:
- `ip link delete outline-tun0` — if not present, returns error 1. Script ignores.
- `ip route flush table 233` — if empty, no-op.
- `ip rule del priority 23333` — if absent, errors. Script ignores.
- `sysctl -w` — always succeeds if key exists.
- resolv.conf restore — checks first, no-op if already restored.

## After successful revert

`ouf-panic` deletes `snapshot.json` last. Absence of the file is the "clean" indicator. `ouf status` checks for the file's presence as a "dirty" flag.
