# mcs-bootloader

Keep a heavy Minecraft server asleep when nobody is playing. A small Go gateway stays online, wakes Minecraft on a login attempt, and gracefully stops it after an hour with no players.

**Supported:** Linux with systemd, Minecraft Java **1.21.1 / protocol 767**, including modded servers. This is a TCP gateway: once Minecraft is ready, the original handshake and all game/mod traffic pass through unchanged.

- Server-list refreshes show status without starting Minecraft.
- Joining while asleep starts the server once and shows a **“server booting, please reconnect”** message. Reconnect when it is ready; there is no simulated lobby or indefinite loading screen.
- Shutdown uses loopback RCON `stop`. Unknown player counts and active joins prevent sleep. The backend has no restart loop and is never force-killed.
- Logs cover state changes and useful errors; routine polling and status pings are quiet.

## Requirements

- An existing, working Minecraft server and the Java/runtime required by its modpack.
- Linux with systemd and journald namespace support, Python **3.10+**, [Task 3](https://taskfile.dev/docs/installation), `sudo`, GNU `tar`, `zstd`, and standard system tools (`runuser`, `sha256sum`).
- A non-root account that owns the Minecraft files, plus an operator account with sudo access. Tasks use noninteractive sudo; run `sudo -v` first if your setup requires a password.
- For **source builds only**: [Go 1.24+](https://go.dev/dl/), Git, and a C compiler for the race tests. Release packages include the compiled binary.

The installer manages one instance using `/opt/mcs-bootloader`, `/etc/mcs-bootloader`, and two systemd units. Minecraft can live anywhere the chosen service account can access.

## Install

**Stop Minecraft and its old launcher/restart loop first.** Keep them stopped until installation finishes. The installer verifies a full backup before changing any Minecraft settings.

From source:

```sh
git clone https://github.com/difof/mcs-bootloader.git
cd mcs-bootloader
task config:new
task config:edit
# Set the account, server root, launch command and backup destination below.
task backup
task install
```

For a prebuilt install, download the appropriate `linux_amd64` or `linux_arm64` archive and `SHA256SUMS` from [Releases](https://github.com/difof/mcs-bootloader/releases), verify the archive with `sha256sum --check --ignore-missing SHA256SUMS`, and extract it. Inside its directory, run the same configuration and backup tasks, then **`task install:binary`**. Release downloads appear after the first intentional release tag.

`task backup` saves the **whole server**, including mods and custom world directory names, to the new/empty `MC_BACKUP_DIRECTORY`. It verifies the compressed archive and checksum; it never overwrites an existing backup. The automatic backup does not support symlinks pointing outside the server root.

Installation changes only the network/RCON properties required for the gateway. It preserves worlds, mods, authentication and whitelist settings. It starts the gateway; Minecraft remains asleep until a login attempt or `task start`. Forward/expose only the public gateway port (default **25565**).

## Configure `.env`

`task config:new` creates a mode-0600 file with a random RCON password. Set these to match your existing server:

```dotenv
MC_SERVICE_USER=minecraft
MC_SERVER_DIRECTORY=/srv/minecraft
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["-Xms2G","-Xmx6G","-jar","server.jar","nogui"]'
MC_BACKUP_DIRECTORY=/srv/minecraft-backups/pre-bootloader
MC_IDLE_TIMEOUT=1h
```

For Forge/NeoForge, use the **actual argument files from your installed pack**:

```dotenv
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["@user_jvm_args.txt","@libraries/net/neoforged/neoforge/YOUR_VERSION/unix_args.txt","nogui"]'
```

For a custom script:

```dotenv
MC_LAUNCH_EXECUTABLE=/bin/bash
MC_LAUNCH_ARGS='["./start-once.sh","--your-option","value with spaces"]'
```

The working directory is `MC_SERVER_DIRECTORY`. An executable may also be a relative path such as `./start-once.sh`. Arguments are a **JSON array of literal strings**: no shell expansion, interpolation or command substitution. Launchers must stay in the foreground, preferably `exec` the game process, and must not contain their own restart loop.

After installation, project `.env` links to **`/etc/mcs-bootloader/.env`**. All operator settings and the RCON password live there; Minecraft's required native password is synchronized on its next cold start. The file is private to the operator and service account. See [.env.example](.env.example) for every setting.

```sh
task config:edit
task config:check
task config:apply
```

Timers, messages, connection limits and log level apply through the private Unix socket without a restart. Launch executable/arguments require `task gateway:restart` and take effect on Minecraft's next start. Password changes require Minecraft to be stopped before applying. Network addresses and service paths are installation settings; changing them needs a coordinated migration.

## Daily tasks

Run `task` to list everything. Commands run from the project/release directory.

| Command | What it does |
|---|---|
| `task status` | State, players, connections, health and running version |
| `task start` / `task stop` | Wake, or save/stop only when confirmed empty; later logins can wake it again |
| `task command -- op PlayerName` | Send any console command through the private socket/RCON |
| `task command -- 'tellraw @a {"text":"Hello!"}'` | Pass JSON or special characters literally |
| `task logs:server` / `task logs:server:follow` | Read/follow Minecraft console output |
| `task logs` / `task logs:follow` | Read/follow gateway events |
| `task config:show` / `task config:live` | Desired/applied settings, password redacted |
| `task config:edit` / `task config:check` / `task config:apply` | Edit, validate and live-reload `.env` |
| `task gateway:stop` / `task gateway:start` | Disable/restore public access and wake monitoring; Minecraft stays running |
| `task gateway:restart` | Load new gateway code/startup settings; disconnects clients, leaves Minecraft running |
| `task deploy` / `task deploy:binary` | Validate/install a source build or packaged binary, then restart explicitly |
| `task config:init` | Restore the project symlink to an existing installation's `.env` |

For a console-like setup, follow `task logs:server:follow` in one terminal and run `task command -- ...` in another. There is no tmux stdin to attach to. RCON commands are never automatically retried: if a reply is lost, check state before repeating a command with side effects.

Both services run as `MC_SERVICE_USER`, never root. Operator tasks use `sudo -u` to access that user's mode-0600 socket. The daemon's sudo rule allows exactly **starting its backend unit**; it cannot run arbitrary root commands. RCON and the backend bind only to loopback.

## Updates and releases

For source updates: `git pull`, `task deploy`, then `task gateway:restart`. For a release archive, extract it separately, run `task config:init`, `task deploy:binary`, then `task gateway:restart`. Neither deployment command restarts Minecraft.

`task check` runs formatting, vet, Go race tests and Python tests. `task release:package VERSION=v0.2.1` builds local packages; it does **not** tag or publish.

GitHub releases run only for intentionally pushed SemVer tags such as `v0.2.1`. One bounded Linux job tests once, cross-compiles amd64/arm64, and publishes archives, checksums, build information and release notes/changelog. Ordinary branch pushes and pull requests spend **no Actions minutes** in this repository. Do not create a release tag for routine edits; [AGENTS.md](AGENTS.md) records the explicit owner-approval policy.
