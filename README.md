# mcs-bootloader

Let a heavy Minecraft server sleep when nobody is playing. A small Go gateway stays online, wakes Minecraft on an allowed login, and saves/stops it after **15 minutes idle** by default.

**Linux/systemd · Minecraft Java 1.21.1 / protocol 767 · modded servers supported.** Once ready, the original handshake and game/mod traffic pass through unchanged. Server-list pings do not wake it. A join during startup gets a reconnect message with an approximate countdown learned from previous successful boots.

Shutdown uses loopback RCON `stop`. Unknown player counts and active joins prevent sleep; there is no backend restart loop or force-kill. Native Minecraft whitelist policy controls admission. Optional Prometheus metrics cover the gateway, Minecraft, RCON, players, JVM, uptime, boot timing and watched folders.

## Install

Download the executable for your architecture from [Releases](https://github.com/difof/mcs-bootloader/releases), verify it against `SHA256SUMS`, and rename it to `mcs-bootloader`.

Requirements: an existing working Minecraft server and Java, Linux with systemd/journald namespaces and `busctl`, Python **3.10+**, `sudo`, `runuser`, `visudo`, `getconf`, and a non-root account that owns the server. Release users need no Go, Task, Git or source checkout: operator scripts and service templates are embedded.

Stop Minecraft and its old launcher/restart loop first, then:

```sh
chmod +x ./mcs-bootloader
./mcs-bootloader init
./mcs-bootloader config edit
# Set MC_SERVICE_USER, MC_SERVER_DIRECTORY, MC_LAUNCH_EXECUTABLE and MC_LAUNCH_ARGS.
./mcs-bootloader install
```

The installer configures the backend and RCON on loopback and starts the gateway. Minecraft stays asleep until a login or `start`. Forward only the public gateway port, default **25565**. The installed command is `/opt/mcs-bootloader/bin/mcs-bootloader`; put it on your PATH for the examples below. See [installation and updates](docs/install.md) for details.

## Everyday commands

```sh
mcs-bootloader status
mcs-bootloader start
mcs-bootloader command -- op PlayerName
mcs-bootloader logs --server --follow
mcs-bootloader stop
mcs-bootloader stats
```

`stop` saves and shuts down only when the server is confirmed empty, with no active joins. Later logins can wake it again. `gateway stop` stops public access and wake monitoring while Java stays running. Human output supports terminal colors; append `--json` for machine output, including structured log records.

All operator settings, including the RCON password, live in the private `.env`. Edit and apply supported settings without a gateway restart:

```sh
mcs-bootloader config edit
mcs-bootloader config check
mcs-bootloader config reload /etc/mcs-bootloader/.env
```

Reload validates the complete file and applies it atomically over the private socket. It never writes settings back to disk. The [complete example](.env.example) includes launch arguments, idle timers, reconnect/restart messages, MOTD overrides and metrics options.

## Guides

| Guide | Covers |
| --- | --- |
| [Install](docs/install.md) | Dependencies, first setup, custom launchers, updates and removal |
| [CLI and Task](docs/cli.md) | Full command reference, console access, JSON/color and development wrappers |
| [Configuration](docs/configuration.md) | `.env`, live reload, MOTD, whitelist and startup estimates |
| [Lifecycle and access](docs/lifecycle.md) | Sleep/wake behavior, process ownership and troubleshooting |
| [Metrics](docs/metrics.md) | Prometheus, server uptime, startup timing, JVM/player data and folder sizes |

## From source

Use Go **1.24+**, Python 3.10+ and [Task 3](https://taskfile.dev/). Race tests also need a C compiler. Every operator task builds current source automatically using Go's compiler cache.

```sh
task check
task status -- --json
task command -- op PlayerName
task config:reload -- .env
task logs:server:follow
task cli -- help
```

`task` lists tasks; [CLI and Task](docs/cli.md#source-development-with-task) maps every command. Local packaging and the Actions budget policy are documented in [AGENTS.md](AGENTS.md).
