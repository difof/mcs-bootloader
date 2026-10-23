# mcs-bootloader

Let a heavy Minecraft server sleep when nobody is playing. A small Go gateway stays online, wakes Minecraft on an allowed login, and gracefully stops it after a configurable idle period (default **1 hour**).

**Linux/systemd · Minecraft Java 1.21.1 / protocol 767 · modded servers supported.** The original handshake and game/mod traffic pass through unchanged once Minecraft is ready. Server-list pings do not wake it. A join while asleep starts it once and shows a “server booting, please reconnect” message.

Shutdown uses loopback RCON `stop`; unknown player counts and active joins prevent sleep. No backend restart loop or force-killing. Native Minecraft whitelist policy controls admission. Optional Prometheus metrics cover the gateway, Minecraft, RCON, players, JVM and watched folders.

## Requirements

- An existing working Minecraft server and its required Java/runtime.
- Linux with systemd/journald namespaces, `busctl`, Python **3.10+**, `sudo`, GNU `tar`, `zstd`, `runuser`, `sha256sum` and `getconf`.
- An existing non-root account that owns Minecraft, and an operator with sudo access. Run `sudo -v` first if your setup requires a password; automated operations use noninteractive sudo.

**Release users need one executable. No Task, Go, Git or source download.** Python helpers, service templates and configuration defaults are embedded in it. The installer manages one instance under `/opt/mcs-bootloader` and `/etc/mcs-bootloader`; the server directory and launch command are configurable.

## Install

Download the `linux_amd64` or `linux_arm64` executable and `SHA256SUMS` from [Releases](https://github.com/difof/mcs-bootloader/releases). Verify with `sha256sum --check --ignore-missing SHA256SUMS`, then rename the executable to `mcs-bootloader`.

**Stop Minecraft and its old launcher/restart loop first.** Keep both stopped through backup and installation.

```sh
chmod +x ./mcs-bootloader
./mcs-bootloader init
./mcs-bootloader config edit
# Set your service account, server directory, launch args and backup destination.
./mcs-bootloader backup
./mcs-bootloader install
```

`init` creates a private `.env` with a random RCON password and refuses to overwrite an existing file. `backup` saves and verifies the **whole server**, including mods and custom world directories, in a new/empty `MC_BACKUP_DIRECTORY`. External symlinks are not supported by the backup routine. Installation requires the verified backup and changes only the required network/RCON settings; it preserves worlds, mods, authentication and whitelist settings.

For a later backup, select a new/empty backup destination, run `stop`, wait for Minecraft to finish saving, and run `gateway stop` before `backup`. Restart the gateway afterwards with `gateway start`; backup refuses an active gateway so a join cannot wake Java during the archive.

Installation starts the gateway; Minecraft stays asleep until a login or `start`. Forward only the public gateway port (default **25565**). Use `/opt/mcs-bootloader/bin/mcs-bootloader` afterwards, or put a symlink to it in your PATH. Examples below assume that command is on your PATH.

## Configure

All settings, including the RCON password, live in one private `.env`. Before installation, commands find `.env` in the current directory; after installation the canonical file is `/etc/mcs-bootloader/.env`. Use **`--env /absolute/path/.env`** to select a file explicitly. The original installation config path links to the canonical file.

```dotenv
MC_SERVICE_USER=minecraft
MC_SERVER_DIRECTORY=/srv/minecraft
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["-Xms2G","-Xmx6G","-jar","server.jar","nogui"]'
MC_BACKUP_DIRECTORY=/srv/minecraft-backups/pre-bootloader
MC_IDLE_TIMEOUT=1h
```

For Forge/NeoForge, use the actual argument file paths from your pack:

```dotenv
MC_LAUNCH_ARGS='["@user_jvm_args.txt","@libraries/net/neoforged/neoforge/YOUR_VERSION/unix_args.txt","nogui"]'
```

Custom executables/scripts work too, for example `MC_LAUNCH_EXECUTABLE=/bin/bash` with `MC_LAUNCH_ARGS='["./start-once.sh","--your-option","value with spaces"]'`. The working directory is `MC_SERVER_DIRECTORY`. Arguments are literal strings in a JSON array; no shell expansion. Scripts must stay in the foreground, preferably `exec` the game process, and must not restart it themselves.

```sh
mcs-bootloader config edit
mcs-bootloader config check
mcs-bootloader config reload /etc/mcs-bootloader/.env
```

Reload resolves the path and sends its digest over the private Unix socket. The daemon validates and atomically applies that snapshot without writing `.env`. The supplied path must resolve to its current canonical file. Invalid values or an unavailable metrics port preserve the applied settings.

Timers, messages, limits, MOTDs, metrics and log level apply live. Launch-command changes need `gateway restart` and affect Minecraft's next start. Password changes require Minecraft to be stopped. Network addresses and service paths require a coordinated installation migration. See [.env.example](.env.example) for every setting; `config show` and `config live` redact the password.

### MOTD and whitelist

```dotenv
MC_OVERRIDE_MOTD=true
MC_MOTD="Our modded server\nJoin to wake it up"
MC_RESTART_MESSAGE="Gateway is restarting. Please reconnect in a moment."
MC_RESTART_GRACE=3s
```

The MOTD override also works while Minecraft is running and preserves other status/mod metadata. Controlled gateway stops/restarts ask Minecraft to send the configured disconnect reason before closing relays; Java stays running. A crash or unavailable RCON cannot guarantee delivery.

```sh
mcs-bootloader command -- whitelist add PlayerName
mcs-bootloader command -- whitelist reload
mcs-bootloader command -- whitelist on
```

The gateway reads `white-list` from `server.properties` and entries from `whitelist.json`; native changes take effect on the next login/metrics poll. No separate `.env` whitelist toggle. Enabled but unreadable/malformed policy rejects admission without waking Minecraft. The pre-login check uses the claimed name; Minecraft performs final authentication and admission.

### Prometheus and folder sizes

```dotenv
MC_METRICS_ENABLED=true
MC_METRICS_LISTEN=127.0.0.1:9464
MC_METRICS_POLL_INTERVAL=15s
MC_METRICS_PLAYER_INTERVAL=60s
MC_METRICS_PLAYER_DETAILS=true
MC_METRICS_DIRECTORIES='{"world":"/srv/minecraft/world","backups":"/srv/minecraft-backups"}'
MC_METRICS_DIRECTORY_INTERVAL=5m
MC_METRICS_DIRECTORY_TIMEOUT=30s
```

Run `config reload`, then scrape **`http://127.0.0.1:9464/metrics`**. Use `stats` for a compact summary, `metrics` for the full table, `metrics --raw` for Prometheus text, or `metrics --json` for structured samples. This listener is unauthenticated; loopback is the default. Player details expose names/UUIDs and supported health/food/XP/position data; switch them off when unwanted.

Folder names are stable metric labels. Paths must be absolute and readable by the service account. Scans run separately from Minecraft polling, skip symlinks, count hardlinked files once per watched root, and publish logical/allocated bytes, file/directory counts and scan health. Failed or incomplete scans retain explicitly marked last-good totals. No file contents are read. Scrapes use cached data and never wake Minecraft. [Metrics reference](docs/metrics.md) covers availability, freshness and JVM collection.

## Commands

Run `mcs-bootloader --help` or `<command> --help`. Output is readable by default and colored in a terminal. `--color auto|always|never` controls it; `NO_COLOR` disables automatic color. **`--json`** selects machine output, including errors; logs stream one JSON object per line.

| Command | Purpose |
|---|---|
| `status` / `stats` | Lifecycle/player health and cached telemetry |
| `start` / `stop` | Wake once, or save/stop when confirmed empty; later logins can wake it |
| `command -- op PlayerName` | Send a console command through the private socket/RCON |
| `command -- 'tellraw @a {"text":"Hello!"}'` | Send JSON/special characters literally |
| `logs` / `logs --follow` | Read/follow gateway events |
| `logs --server --follow` | Follow Minecraft console output |
| `metrics --raw` / `metrics --json` | Cached Prometheus text or structured samples |
| `config show` / `config live` | Desired/applied settings, with secrets redacted |
| `config edit` / `config check` / `config reload [path]` | Edit, validate or live-reload `.env` |
| `init [path]` / `config link [path]` | Create first config or link to an existing installation |
| `gateway start` / `gateway stop` / `gateway restart` | Control public access and wake monitoring; keep Java running |
| `deploy` | Validate and atomically install this executable; restart explicitly |
| `gateway configure` | Refresh service settings without restarting processes |
| `remove` / `remove --yes` | Preview or remove owned installation files; keep server, backups and config |
| `version` | Show this executable's version; `status` shows the daemon's version |

Follow `logs --server --follow` in one terminal and use `command -- ...` in another. There is no tmux stdin to attach to. Commands are never automatically retried; check state before repeating a command whose reply was lost.

Both services run as `MC_SERVICE_USER`. When needed, the CLI briefly runs through `sudo` to access that account's private socket or manage system services; it can run from a private download directory. The daemon's sudo rule permits only starting its backend unit; RCON and the Minecraft backend stay on loopback.

To update, download/verify the new binary, run `./mcs-bootloader deploy`, then `mcs-bootloader gateway restart`. Removal refuses a running or uninspectable backend and preserves your server files, backups and private configuration. It also leaves native Minecraft network settings unchanged, so configure those before running the server without the gateway. Review the preview before using `--yes`.

## From source

Development needs [Go 1.24+](https://go.dev/dl/), Git, a C compiler for race tests, and optionally [Task 3](https://taskfile.dev/docs/installation).

```sh
git clone https://github.com/difof/mcs-bootloader.git
cd mcs-bootloader
task build
task init
task config:edit
task backup
task install
```

Task wraps Go development tools and the built binary. Run `task build` after source edits. `task check` runs formatting, vet, race tests and Python tests. `task deploy` checks/builds/deploys; `task gateway:restart` then loads the update.

| Task | Binary equivalent |
|---|---|
| `task status -- --json` | `mcs-bootloader status --json` |
| `task command -- op PlayerName` | `mcs-bootloader command -- op PlayerName` |
| `task config:reload -- .env` | `mcs-bootloader config reload .env` |
| `task logs:server:follow` | `mcs-bootloader logs --server --follow` |
| `task deploy:binary` / `task install:binary` | `mcs-bootloader deploy` / `mcs-bootloader install` |
| `task cli -- <arguments>` | Any binary command, preserving arguments literally |

`task` lists the remaining wrappers. `task release:package VERSION=vX.Y.Z` builds release binaries locally; it never tags or publishes. Release Changelog entries come from committed subjects/links since the previous reachable stable tag. The [release policy](AGENTS.md) requires deliberate owner-approved tags and keeps Actions to one short Linux job.
