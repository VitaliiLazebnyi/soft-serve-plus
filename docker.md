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
  --init \
  -e SOFT_SERVE_INITIAL_ADMIN_KEYS="YOUR_ADMIN_KEY_HERE" \
  --restart unless-stopped \
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

Or by using docker-compose:

```yaml
---
services:
  soft-serve-plus:
    image: ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
    container_name: soft-serve-plus
    init: true
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

For why the examples use `--init` and `init: true`, see
[Reaping background git processes](#reaping-background-git-processes).

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

Git leaves orphaned processes behind: since git 2.47, one
`git maintenance run --auto --detach` for each push that updates a ref, plus a
helper for each transfer that is cut off part-way. The kernel re-parents each
orphan to PID 1 of the container, which must reap it when it exits. If nothing
does, it stays a zombie (`<defunct>`) process that counts against the
container's PID limit (`--pids-limit`, `pids_limit:` in docker-compose) until
the server can no longer start git.

The image already handles this in two ways:

- Its entrypoint is `/sbin/tini -s -- /usr/local/bin/soft serve`, so
  [tini](https://github.com/krallin/tini) is PID 1 and reaps the orphans.
  `-s` makes tini a child subreaper, so it still reaps them when another init
  is PID 1.
- When `soft` itself runs as PID 1, it starts the server again as a child,
  forwards signals to it and reaps every process that exits. It logs
  `soft is PID 1: running the server as a child process ...` when it does
  this. This costs a second `soft` process, so an init is still preferred. If
  it cannot start the child, it logs the warning
  `soft is PID 1 and cannot supervise itself ...` and the server runs without
  anything reaping its orphans.

The examples above still add `--init` (`init: true`). With this image it is a
safeguard rather than a requirement, and it works with `read_only: true`,
`cap_drop: [ALL]` and `no-new-privileges`. When the entrypoint is overridden
(for example `--entrypoint /usr/local/bin/soft`) or you run `soft serve` from
your own image, `soft` is PID 1 and supervises itself, so the orphans are
reaped anyway; the init saves the second `soft` process and also covers the
case where the child cannot be started. It is required only with an older
image that has neither tini nor this fallback.

A PID limit such as `pids_limit: 128` can stay: with an init, the container
uses about 10 processes and the count stays stable.

### Kubernetes

A container's `command:` replaces the image's entrypoint, and so removes tini.
Pass options with `args:` instead, which are appended to
`/usr/local/bin/soft serve`. If you do set `command:`, keep tini in front of it:

```yaml
command: ["/sbin/tini", "-s", "--", "/usr/local/bin/soft", "serve"]
```

or set `shareProcessNamespace: true` on the pod, which makes the pod's pause
container PID 1 and the reaper. Otherwise `soft` falls back to supervising
itself as described above.

### Not starting background maintenance

Optionally, tell git to run automatic maintenance in the foreground instead of
detaching it, which removes the orphan each push leaves behind. Add the setting
through git's `GIT_CONFIG_*` variables (if you already set some, append to them
and raise `GIT_CONFIG_COUNT`):

```yaml
    environment:
      GIT_CONFIG_COUNT: "1"
      GIT_CONFIG_KEY_0: gc.autoDetach
      GIT_CONFIG_VALUE_0: "false"
```

`maintenance.autoDetach` falls back to `gc.autoDetach` when it is not set, so
this one key covers git 2.47 and later as well as older versions.

The trade-off is that a push then waits for maintenance to finish whenever it
has real work to do. The setting reaches the git processes that serve pushes;
mirror syncs and imports set their own `GIT_CONFIG_*` variables and do not pick
it up. It does not cover interrupted transfers either, so it does not
replace an init. The server does not set these keys itself.

### Checking for zombies

Zombie processes show the state `Z`. This lists them inside the container, and
prints nothing when there are none:

```sh
docker exec soft-serve-plus ps -o stat,pid,comm | grep '^Z'
```

With an init, the number of processes in the container
(`docker exec soft-serve-plus ps | wc -l`) stays small and stable however many
pushes the server handles.

[ghcr]: https://github.com/VitaliiLazebnyi/soft-serve-plus/pkgs/container/soft-serve-plus

> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.
