#!/bin/sh
# sshd runs this as `/bin/sh -c /opt/bitc/sbin/bitc-login` with SSH_ORIGINAL_COMMAND set
[ -z "${SSH_AUTH_SOCK-}" ] || exit 64          # a forwarded agent never reaches the gate
exec /usr/bin/sudo -n /opt/bitc/sbin/bitc-gate "${SSH_ORIGINAL_COMMAND-}"
