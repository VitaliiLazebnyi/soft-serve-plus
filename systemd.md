# Running Soft Serve Plus as a Systemd Service

Most Linux OSes use Systemd as an init system and service management. You can
use Systemd to manage Soft Serve Plus as a service on your host machine.

The Soft Serve Plus deb/rpm packages come with Systemd service files
pre-packaged. You can download them from the
[releases page](https://github.com/VitaliiLazebnyi/soft-serve-plus/releases).
Follow the [installation instructions](https://github.com/VitaliiLazebnyi/soft-serve-plus#installation)
for more information.

## Writing a Systemd Service File

> **Note** you can skip this section if you installed Soft Serve Plus from our
> deb/rpm packages.

Start by writing a Systemd service file to define how your Soft Serve Plus server
should start.

First, we need to specify where the data should live for our server. Here I
will be choosing `/var/local/lib/soft-serve` to store the server's data. Soft
Serve will look for this path in the `SOFT_SERVE_DATA_PATH` environment
variable.

Soft Serve Plus does not need root, so we will run it as a dedicated
unprivileged `soft-serve` system user. Create the user, then create the data
directory owned by it before proceeding. It will hold the repositories, the
database, the SSH host keys and the logs, so make it readable only by the
`soft-serve` user. Soft Serve Plus creates a missing data directory with mode
`0700`, but it keeps the mode of a directory that already exists, so set it
here. `install -d -m 700` also fixes the mode and owner of a directory you
created before.

```sh
sudo useradd --system --user-group --home-dir /var/local/lib/soft-serve \
  --no-create-home --shell /usr/sbin/nologin soft-serve
sudo install -d -m 700 -o soft-serve -g soft-serve /var/local/lib/soft-serve
```

We will also create a `/etc/soft-serve.conf` file for any extra server settings
that we want to override. It can hold secrets such as a database password, so
make it readable only by `root`. systemd reads it as `root`, so the server does
not need access to it.

```sh
sudo touch /etc/soft-serve.conf
sudo chmod 600 /etc/soft-serve.conf
```

```conf
# Config defined here will override the config in /var/local/lib/soft-serve/config.yaml
# Keys defined in `SOFT_SERVE_INITIAL_ADMIN_KEYS` will be merged with
# the `initial_admin_keys` from /var/local/lib/soft-serve/config.yaml.
#
#SOFT_SERVE_GIT_LISTEN_ADDR=:9418
#SOFT_SERVE_HTTP_LISTEN_ADDR=:23232
#SOFT_SERVE_SSH_LISTEN_ADDR=:23231
#SOFT_SERVE_SSH_KEY_PATH=ssh/soft_serve_host_ed25519
#SOFT_SERVE_INITIAL_ADMIN_KEYS='ssh-ed25519 AAAAC3NzaC1lZDI1...'
```

> **Note** Soft Serve Plus stores its server configuration and settings in
> `config.yaml` under its _data path_ directory specified using
> `SOFT_SERVE_DATA_PATH` environment variable.

Now, let's write a new `/etc/systemd/system/soft-serve.service` Systemd service file:

```conf
[Unit]
Description=Soft Serve Plus git server 🍦
Documentation=https://github.com/VitaliiLazebnyi/soft-serve-plus
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=soft-serve
Group=soft-serve
Restart=always
RestartSec=1
ExecStart=/usr/bin/soft serve
Environment=SOFT_SERVE_DATA_PATH=/var/local/lib/soft-serve
EnvironmentFile=-/etc/soft-serve.conf
WorkingDirectory=/var/local/lib/soft-serve
# Keep the files the server creates (repositories, database, logs) out of
# reach of other local users.
UMask=0027

# Hardening
ReadWritePaths=/var/local/lib/soft-serve
NoNewPrivileges=true
LimitNOFILE=1048576
ProtectSystem=strict
ProtectHome=true
PrivateUsers=yes
PrivateTmp=true
PrivateDevices=true
ProtectHostname=true
ProtectClock=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictRealtime=true
RestrictSUIDSGID=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
# Fail a filtered system call with EPERM instead of killing the process with
# SIGSYS, so git, hooks and soft report an error rather than dying or hanging.
SystemCallErrorNumber=EPERM
SystemCallFilter=@system-service
# @resources (nice, setpriority, setrlimit, sched_setaffinity, ...) is allowed:
# git and repository hooks may use it, and without capabilities it cannot
# raise priorities or hard limits anyway.
SystemCallFilter=~@privileged
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
```

Great, we now have a Systemd service file for Soft Serve Plus. It is the same
unit the deb/rpm packages install, apart from the data path. It runs Soft Serve
Plus as the unprivileged `soft-serve` user and sandboxes it: the file system is
read-only except for the data directory (`ReadWritePaths=`), and the service has
no capabilities, so it can only listen on unprivileged ports (1024 and above,
which includes the defaults). If you move the data directory or need the server
to write anywhere else, update `ReadWritePaths=` too. If you previously ran Soft
Serve Plus as `root`, hand the existing data over to the new user with
`sudo chown -R soft-serve:soft-serve /var/local/lib/soft-serve`. The settings defined here may vary depending on your
specific setup. For more information on Systemd service files, refer to
[systemd.service](https://www.freedesktop.org/software/systemd/man/systemd.service.html)
and [systemd.exec](https://www.freedesktop.org/software/systemd/man/systemd.exec.html).

## Start Soft Serve Plus on boot

Now that we have our Soft Serve Plus Systemd service file in-place, let's go ahead
and enable and start Soft Serve Plus to run on-boot.

```sh
# Reload systemd daemon
sudo systemctl daemon-reload
# Enable Soft Serve Plus to start on-boot
sudo systemctl enable soft-serve.service
# Start Soft Serve Plus now!!
sudo systemctl start soft-serve.service
```

You can monitor the server logs using `journalctl -u soft-serve.service`. Use
`-f` to _tail_ and follow the logs as they get written.
