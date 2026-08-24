# vim:ft=Dockerfile
# Pinned to a release whose rsync is still 3.4.x. "fedora" (latest) is now
# Fedora 44, which ships rsync 3.5.0, whose hardened path traversal
# (RsyncProject/rsync#1050) fails change_dir with "Permission denied (13)"
# against the test tmp dirs. Fedora 42 stays on rsync 3.4.1.
FROM fedora:42

# Install rsync (for running tests).
RUN dnf -y update && dnf -y install rsync openssh-clients go && dnf clean all

# Enable toolchain management (and the module proxy, which is a requirement) so
# that Go 1.23 (from Fedora) will use Go 1.24 for gokrazy/rsync (or whichever
# version we specify as language/toolchain version in our go.mod).
RUN go env -w \
    GOPROXY=https://proxy.golang.org,direct \
    GOSUMDB=sum.golang.org \
    GOTOOLCHAIN=auto
