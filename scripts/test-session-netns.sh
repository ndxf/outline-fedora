#!/bin/bash
# test-session-netns.sh: run the internal/tun Session e2e test inside a
# rootless user+mount+net namespace so tun/route/rule/sysctl mutations
# are isolated from the host.

set -eu
cd "$(dirname "$0")/.."

# Build the test binary in the host env, then execute it inside the
# netns. We compile with -c because `go test` itself wants to fork
# toolchain helpers that don't work well under a rootless netns.
go test -c -o /tmp/tun-session-test ./internal/tun

# The e2e test looks for OUF_TEST_IN_NETNS=1 and skips otherwise.
exec unshare -Umnr --propagation=private -- env \
    OUF_TEST_IN_NETNS=1 \
    /tmp/tun-session-test -test.v "$@"
