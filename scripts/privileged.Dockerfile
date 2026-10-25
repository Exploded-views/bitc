# The privileged suite's image: the pinned toolchain image plus what an ssh host has (sshd, sudo,
# nftables, iproute2, cron and at for the denied-by-allow-list checks). Built by privileged.sh.
ARG BASE
FROM ${BASE}
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d \
 && apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    openssh-server openssh-client sudo nftables iproute2 cron at procps psmisc python3 git util-linux bash coreutils \
 && rm -rf /var/lib/apt/lists/*
