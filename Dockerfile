# Pinned by digest so a rebuild gets the same base; Dependabot bumps it.
FROM alpine:3.24@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a

RUN apk add --no-cache git bash openssh tini

# Run as an unprivileged user. The data directory belongs to it, so a new
# named volume mounted there does too.
RUN addgroup -S -g 1000 soft \
  && adduser -S -D -u 1000 -G soft -h /home/soft -s /sbin/nologin soft \
  && mkdir -p /soft-serve \
  && chown soft:soft /soft-serve

# Create directories
WORKDIR /soft-serve
# Expose data volume
VOLUME /soft-serve

# Environment variables
ENV SOFT_SERVE_DATA_PATH="/soft-serve"
# workaround to prevent slowness in docker when running with a tty
ENV CI="1"

# Expose ports
# SSH
EXPOSE 23231/tcp
# HTTP
EXPOSE 23232/tcp
# Git
EXPOSE 9418/tcp
# The stats (Prometheus metrics) server listens on localhost:23233 by default,
# which is not reachable from outside the container. See docker.md to expose it.

# Run soft under tini so it is not PID 1. Git starts detached background
# processes (e.g. "git maintenance run --auto --detach" after a push or fetch).
# When the git process that started one exits, the orphan is re-parented to
# PID 1, which must reap it when it exits in turn; tini does, soft would not.
# -s registers tini as a child subreaper, so it still reaps them (and does not
# warn) when the container is started with --init and tini is not PID 1.
ENTRYPOINT [ "/sbin/tini", "-s", "--", "/usr/local/bin/soft", "serve" ]

# Healthy once the HTTP server answers and the database is reachable. Override
# it (--health-cmd) or turn it off (--no-healthcheck) if you move or disable
# the HTTP server.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:23232/readyz || exit 1

USER soft:soft

# GoReleaser puts the binary for each platform under <os>/<arch>[/<variant>]/.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/soft /usr/local/bin/soft
