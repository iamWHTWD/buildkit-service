#!/bin/sh
set -eu

# apt-cacher-ng still uses select(2)/fd_set internally. With the default
# Kubernetes/containerd nofile limit (often 1048576), the process can receive
# file descriptors above FD_SETSIZE (1024) and abort with:
#   bit out of range 0 - FD_SETSIZE on fd_set
# Clamp nofile so overload becomes a refused/retried connection instead of a
# process crash and potentially corrupted partial cache files.
ulimit -n "${ACNG_NOFILE_LIMIT:-1024}"

exec /usr/sbin/apt-cacher-ng -c /etc/apt-cacher-ng ForeGround=1
