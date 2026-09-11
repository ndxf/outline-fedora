# outline-fedora

A native CLI Outline VPN client for Fedora. Built because the upstream Electron [Outline client](https://github.com/Jigsaw-Code/outline-apps) (both the AppImage and the Flathub build) exits silently within ~250ms on Fedora 44 with no crash log, no stderr output, and no way to debug — leaving Fedora users with no working Outline client.

This project is not affiliated with Jigsaw. It uses Jigsaw's Apache-2.0 [outline-sdk](https://github.com/Jigsaw-Code/outline-sdk) for the Shadowsocks transport, and ports the Linux TUN/routing/DNS glue from `outline-sdk/x/examples/outline-cli`.

## Status

Early. Works end-to-end in isolated netns tests; **has not yet been used against a real Outline server on a real workstation.** Do not install this as your only VPN client until you've smoke-tested it in a VM and are comfortable with the "undo" story below.

## What it does

Same behavior as the official Outline GUI client:

- Full-tunnel VPN via a TUN device — no per-app configuration, everything routes through the remote Outline server transparently.
- Stores multiple `ss://` access keys with human-readable names, one active at a time. `ouf switch home` / `ouf switch office`.
- Overrides system DNS (default 9.9.9.9) while connected, restores it on disconnect.
- Disables IPv6 while connected to prevent leaks, restores your prior setting on disconnect.

## What's different

- **Snapshot-first design.** Before mutating any host state (TUN, routes, ip rules, DNS, sysctls), the daemon writes a JSON snapshot of the pre-state to `/var/lib/outline-fedora/snapshot.json`. A pure shell script `ouf-panic` reads that snapshot and reverts, without needing the Go daemon to be alive. **If anything goes wrong you can always get back to a working network.**
- Three ways to revert:
  - `ouf disconnect` (normal path).
  - `ouf undo` (uses daemon if reachable, falls back to invoking `ouf-panic` directly).
  - `sudo /usr/local/sbin/ouf-panic` (the "if all else is broken" button — plain bash, no Go dependency, no daemon needed).
- `systemctl stop oufdee` runs `ouf-panic` via `ExecStopPost=` so `systemctl` alone reverts.
- `Restart=no` — a crashed daemon does NOT auto-reconnect. You look, then run `ouf undo` and retry.

## Install

Requires: Fedora 44+, Go 1.22+, root access (for the systemd unit), `jq`, `ip`, `sysctl` (all standard on Fedora).

```
git clone git@github.com:ndxf/outline-fedora.git
cd outline-fedora
sudo ./scripts/install.sh
```

The install script builds the binaries, copies them under `/usr/local/{bin,sbin}`, installs the systemd unit, and creates state directories. It does **not** start or enable the service — you do that yourself:

```
sudo systemctl start oufdee            # start daemon
ouf ping                                # confirm daemon reachable

ouf add ss://<your-access-key>#Home    # add a key
ouf list                                # confirm
ouf connect                             # connect via the active (only) key
ouf status                              # confirm connected
```

To enable at boot (only after you've verified it works):
```
sudo systemctl enable oufdee
```

## Usage

```
ouf add <ss://...> [--name NAME]      # add an access key
ouf list                              # show all keys
ouf remove <name>
ouf rename <old> <new>
ouf use <name>                        # set default without connecting
ouf connect [name]                    # connect (uses active if no arg)
ouf switch <name>                     # alias for connect
ouf disconnect
ouf status
ouf undo                              # revert host state
ouf ping                              # test daemon liveness
```

`ouf connect <name>` with an already-active session will atomically switch: disconnect old key, then connect new key.

## Uninstall

```
sudo ./scripts/uninstall.sh           # keep /etc/outline-fedora/keys.json
sudo ./scripts/uninstall.sh --purge   # also delete stored keys
```

## If the machine ever loses connectivity

`sudo /usr/local/sbin/ouf-panic` reverts all host state to what it was before you connected: deletes the TUN, flushes our routing table, removes our ip rule, restores `/etc/resolv.conf` from the backup (verified by SHA-256), restores the IPv6 sysctl. Safe to run when nothing is broken (no-op), safe to run twice. Also safe if the daemon is dead, missing, or in an unknown state — the script only depends on `ip`, `jq`, `sysctl`, `sha256sum`, `cp`, `ln`, `rm`.

## Architecture

- **`ouf`** — unprivileged user CLI. Sends JSON-per-line RPCs over the Unix socket at `/run/outline-fedora.sock` (group `wheel`).
- **`oufdee`** — root daemon. Owns the keystore, the active session, and all host mutations.
- **`ouf-panic`** — pure shell script that reads the snapshot and reverts. The safety net.
- **`internal/snapshot`** — Go writer that produces snapshots the shell script can read. Wire-compatibility with the shell script is proven by tests.
- **`internal/tun`** — the Linux TUN/routing/DNS session logic, adapted from upstream `outline-sdk/x/examples/outline-cli`. Every mutation writes the intent to the snapshot before the netlink call.
- **`internal/keystore`** — persistent list of `ss://` keys with add/remove/rename/use/mark-connected semantics.
- **`internal/rpc`** — wire protocol between `ouf` and `oufdee`.

## Testing

Unit tests: `go test ./...` — no privileges needed, ~1s.

Panic script tests (rootless netns): `./scripts/test-panic-netns.sh` — proves the revert script correctly handles every failure mode.

Session end-to-end (rootless netns): `./scripts/test-session-netns.sh` — proves the full connect→revert cycle inside an isolated netns using a fake `ss://` URL. No real Outline server needed.

## Files it touches (and only these files)

| Path | When | Restored to previous state by |
|---|---|---|
| Kernel TUN device `ouftun0` | connect | `ouf-panic` deletes it |
| Routing table 233 | connect | `ouf-panic` flushes it |
| ip rule at priority 23333 | connect | `ouf-panic` removes it |
| `/etc/resolv.conf` | connect | `ouf-panic` restores from `/var/lib/outline-fedora/backup/resolv.conf` (SHA-verified) or recreates the original symlink |
| `net.ipv6.conf.all.disable_ipv6` | connect | `ouf-panic` restores prior value |
| `/etc/outline-fedora/keys.json` | `add`/`remove`/`rename`/`use` | not touched by revert; persists |
| `/var/lib/outline-fedora/snapshot.json` | during a connect | `ouf-panic` deletes after successful revert |

Nothing else is written. NetworkManager profiles, systemd-resolved config, iptables/nftables rules, firewalld are not modified.

## License

Apache-2.0 (matches outline-sdk).
