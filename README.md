# mcs-bootloader

**Let your Minecraft server sleep. Wake it when someone joins.**

[![Release](https://img.shields.io/github/v/release/difof/mcs-bootloader)](https://github.com/difof/mcs-bootloader/releases)
[![License: AGPL-3.0-only](https://img.shields.io/badge/license-AGPL--3.0--only-blue)](LICENSE)

This README follows `master`. For a released executable, use the documentation at its release tag; the expanded compatibility below is newer than v0.5.0.

A small Go gateway stays online while a heavy Java server rests. An allowed login starts Minecraft; the player gets a reconnect message with an approximate boot countdown. Once ready, the original handshake and game/mod traffic pass through unchanged.

- **Sleep safely.** After 15 minutes empty by default, loopback RCON saves and stops Minecraft. Unknown player counts and pending joins prevent shutdown.
- **Keep your modpack.** Five small login profiles cover stable Java releases from **1.7.10 through 26.1.2**. The relay does not translate gameplay packets; clients still need the matching game and modpack.
- **Operate from one binary.** Initialize, install, send console commands, inspect logs and update configuration without a source checkout. Python helpers and service templates are embedded.
- **See what is happening.** Quiet event logs and optional Prometheus metrics cover the gateway, Minecraft, players, RCON, JVM, boot timing, uptime and watched folders.

Server-list pings never wake Minecraft. Native whitelist settings control the early admission check, and Minecraft remains responsible for final authentication. A booting connection receives a message and reconnects; it is not held in a fake lobby.

## Quick start

You need an existing working Minecraft server, Linux/systemd, Python **3.10+**, and an operator with sudo access. The server runs under its own non-root account. See the [complete dependencies and installation guide](docs/install.md#requirements).

1. Download the executable for your architecture from [Releases](https://github.com/difof/mcs-bootloader/releases), verify it against `SHA256SUMS`, and rename it to `mcs-bootloader`.
2. Stop Minecraft and its old launcher/restart loop.
3. Initialize, configure and install:

```sh
chmod +x ./mcs-bootloader
./mcs-bootloader init
./mcs-bootloader config edit
./mcs-bootloader install
```

Set your paths and service account in `.env`. For example:

```dotenv
MC_SERVICE_USER=minecraft
MC_SERVER_DIRECTORY=/srv/minecraft
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["-Xms2G","-Xmx6G","-jar","server.jar","nogui"]'
MC_PROTOCOL=767
MC_VERSION=1.21.1
MC_IDLE_TIMEOUT=15m
```

Use your modpack's actual launch arguments and [matching protocol number](docs/compatibility.md). `init` generates the RCON password. Installation configures loopback backend/RCON ports and starts the gateway; Minecraft waits for a login or `start`. Forward only the public gateway port, **25565** by default.

The installed executable is `/opt/mcs-bootloader/bin/mcs-bootloader`. Put it on your PATH to use the commands below. Release users need no Go, Task, Git or source files.

## Everyday use

| What you want | Command |
| --- | --- |
| See server state | `mcs-bootloader status` |
| Start Minecraft | `mcs-bootloader start` |
| Send an operator command | `mcs-bootloader command -- op PlayerName` |
| Follow the Minecraft console | `mcs-bootloader logs --server --follow` |
| Save and stop an empty server | `mcs-bootloader stop` |
| Inspect uptime, JVM and folder metrics | `mcs-bootloader stats` |
| Restart the gateway | `mcs-bootloader gateway restart` |

`stop` refuses active or unknown players and pending joins; later logins can wake the server again. A gateway restart disconnects relayed clients with the configured restart reason where RCON is available, while Java keeps running. Human output supports terminal colors and `NO_COLOR`; add `--json` for machine output.

### Edit configuration without restarting

All operator settings, including the RCON password, live in one private `.env`:

```sh
mcs-bootloader config edit
mcs-bootloader config check
mcs-bootloader config reload /etc/mcs-bootloader/.env
```

Reload validates the file and applies supported settings atomically through the private socket. It never writes settings back to disk. Timers, messages, MOTD overrides and metrics are live settings; network/process changes need the restart or migration described in [Configuration](docs/configuration.md). The [complete example](.env.example) lists every setting.

## Guides

| Guide | Find out about |
| --- | --- |
| [Installation](docs/install.md) | Dependencies, setup, custom launchers, updates and removal |
| [CLI and Task](docs/cli.md) | Commands, console access, JSON/color and source wrappers |
| [Configuration](docs/configuration.md) | `.env`, live reload, MOTD, whitelist and boot estimates |
| [Compatibility](docs/compatibility.md) | Version profiles, protocol numbers, modded traffic and tested boundaries |
| [Lifecycle](docs/lifecycle.md) | Sleep/wake behavior, service ownership and troubleshooting |
| [Metrics](docs/metrics.md) | Prometheus, JVM/player data, uptime, boot timing and folder sizes |

## Development

Use Go **1.24+**, Python 3.10+, [Task 3](https://taskfile.dev/) and a C compiler for race tests. Operator tasks build current source automatically using Go's compiler cache.

```sh
task check
task status -- --json
task command -- op PlayerName
task config:reload -- .env
task logs:server:follow
```

`task` lists available tasks; `task cli -- <arguments>` runs any binary command. See the [Task reference](docs/cli.md#source-development-with-task) and [working agreements](AGENTS.md).

## License

Copyright © 2026 difof. Licensed under the **GNU Affero General Public License, version 3 only** (`AGPL-3.0-only`). See [LICENSE](LICENSE). Downloaded executables include the full text: `mcs-bootloader license` or `task license` from source.
