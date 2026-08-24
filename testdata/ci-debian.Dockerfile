# vim:ft=Dockerfile
# Pinned to the current stable release rather than sid: sid ships rsync 3.5.0,
# whose hardened path traversal (RsyncProject/rsync#1050) fails change_dir with
# "Permission denied (13)" against the test tmp dirs. Stable stays on 3.4.x.
FROM debian:trixie

RUN echo force-unsafe-io > /etc/dpkg/dpkg.cfg.d/docker-apt-speedup
# Paper over occasional network flakiness of some mirrors.
RUN echo 'APT::Acquire::Retries "5";' > /etc/apt/apt.conf.d/80retry

# NOTE: I tried exclusively using gce_debian_mirror.storage.googleapis.com
# instead of httpredir.debian.org, but the results (Fetched 123 MB in 36s (3357
# kB/s)) are not any better than httpredir.debian.org (Fetched 123 MB in 34s
# (3608 kB/s)). Hence, let’s stick with httpredir.debian.org (default) for now.

# Install rsync (for running tests).
RUN apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    rsync ssh git ca-certificates build-essential golang-go bmake zlib1g-dev && \
    rm -rf /var/lib/apt/lists/*

# Build openrsync (for running tests).
RUN cd /usr/src && \
    git clone https://github.com/kristapsdz/openrsync && \
    cd /usr/src/openrsync && \
    ./configure && bmake -j8 && bmake install
