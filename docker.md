# Running Soft Serve Plus with Docker

Soft Serve Plus images are published to the GitHub Container Registry at
[ghcr.io/vitaliilazebnyi/soft-serve-plus][ghcr] for `linux/amd64`,
`linux/arm64` and `linux/arm/v7`.

```sh
docker pull ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

Here’s how you might run Soft Serve Plus as a container.  Keep in mind that
repositories are stored in the `/soft-serve` directory, so you’ll likely want
to mount that directory as a volume in order keep your repositories backed up.

```sh
docker run \
  --name=soft-serve-plus \
  --volume /path/to/data:/soft-serve \
  --publish 23231:23231 \
  --publish 23232:23232 \
  --publish 9418:9418 \
  -e SOFT_SERVE_INITIAL_ADMIN_KEYS="YOUR_ADMIN_KEY_HERE" \
  --restart unless-stopped \
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

Or by using docker-compose:

```yaml
---
version: "3.1"
services:
  soft-serve-plus:
    image: ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
    container_name: soft-serve-plus
    volumes:
      - /path/to/data:/soft-serve
    ports:
      - 23231:23231
      - 23232:23232
      - 9418:9418
    environment:
      SOFT_SERVE_INITIAL_ADMIN_KEYS: "YOUR_ADMIN_KEY_HERE"
    restart: unless-stopped
```

The container runs Soft Serve Plus as the unprivileged `soft` user (UID and GID
`1000`), so the data directory must be writable by that user. A new named volume
is set up for it automatically; for a bind mount, give the directory to that
user first:

```sh
sudo chown -R 1000:1000 /path/to/data
```

### Upgrading from an image that ran as root

Images published before this change ran as `root`, so an existing data
directory belongs to `root` and the container stops with a `permission denied`
error on start. Hand the data over to the `soft` user once, either with the
`chown` command above on the host, or, for a named volume, with:

```sh
docker run --rm --user 0:0 --entrypoint chown \
  --volume soft-serve-data:/soft-serve \
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest -R soft:soft /soft-serve
```

To keep running as `root` instead, pass `--user 0:0` (or `user: "0:0"` in
docker-compose).

## Health check

The image declares a Docker health check that requests `/readyz` from the HTTP
server on `127.0.0.1:23232`, which answers once the server is up and its
database is reachable. It tries HTTPS first and then plain HTTP, so it also
works when you turn on TLS for the HTTP server (`SOFT_SERVE_HTTP_TLS_KEY_PATH`
and `SOFT_SERVE_HTTP_TLS_CERT_PATH`). Over HTTPS it does not check the
certificate, which is issued for your public host name rather than `127.0.0.1`.

If you move the HTTP server to another port or turn it off, override the check
with `--health-cmd` (`healthcheck:` in docker-compose) or disable it with
`--no-healthcheck`. For example, with the HTTP server on port 8080:

```sh
docker run ... \
  -e SOFT_SERVE_HTTP_LISTEN_ADDR=":8080" \
  --health-cmd 'wget -q -O /dev/null http://127.0.0.1:8080/readyz || exit 1' \
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

## Metrics

The stats server, which serves Prometheus metrics at `/metrics`, listens on
`localhost:23233` by default. Inside a container that address is not reachable
from outside, so the image does not expose the port. To scrape it from outside
the container, make it listen on every interface and publish the port:

```sh
docker run ... \
  -e SOFT_SERVE_STATS_LISTEN_ADDR=":23233" \
  --publish 127.0.0.1:23233:23233 \
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

> **Warning**
>
> The stats server has no authentication. Publish it only on a trusted network
> (the example above binds it to the host's loopback interface), or keep it
> unpublished and scrape it from a container on the same Docker network.

## Building the image yourself

The `Dockerfile` copies a prebuilt binary from `<os>/<arch>/soft`, so build that
for Linux first:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o linux/amd64/soft ./cmd/soft
docker build --platform linux/amd64 -t soft-serve-plus:latest .
```

## Reaping background git processes

Git starts detached background processes (for example
`git maintenance run --auto --detach` after a push or a mirror sync). When the
git process that started one exits, the orphan is re-parented to PID 1 of the
container, which must reap it when it exits in turn, or such processes pile up
as zombie (`<defunct>`) processes. The image runs Soft Serve Plus under
[tini](https://github.com/krallin/tini) for this reason, so no extra flags are
needed: `docker run --init` and `init: true` in docker-compose are no longer
required with this image. They are harmless if you already use them, because
tini is registered as a child subreaper and reaps the processes itself.

If you build your own image with `soft serve` as the entrypoint, or run an older
image that does not include tini, run the container with an init process:
`docker run --init ...`, or `init: true` for the service in docker-compose.

[ghcr]: https://github.com/VitaliiLazebnyi/soft-serve-plus/pkgs/container/soft-serve-plus

> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.
