# Minimal worker image: distroless/static (CA certs for registry+S3 TLS, but no
# shell, no package manager, no OS) + two static binaries. Nothing else.
FROM gcr.io/distroless/static-debian12:nonroot AS base

# runsc must run as root (it needs privilege); override the distroless nonroot user.
USER 0:0

# the pinned runsc binary (version-matched across all workers for restore-compat)
COPY runsc /usr/local/bin/runsc
# the sandboxd agent
COPY sandboxd /usr/local/bin/sandboxd

ENV SANDBOXD_RUNSC=/usr/local/bin/runsc \
    SANDBOXD_WORK=/work \
    SANDBOXD_ADDR=:8090

ENTRYPOINT ["/usr/local/bin/sandboxd"]
