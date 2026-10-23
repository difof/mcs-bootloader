# mc-gateway

A small Go gateway that keeps a modded Minecraft server visible while its JVM is stopped. A real login attempt wakes the server; an hour of verified emptiness stops it gracefully through local RCON. Minecraft 1.21.1 / protocol 767 is the supported version for this release.

## Player experience

- Server-list refreshes show sleeping/starting/ready status and never wake the JVM.
- Joining a sleeping server starts it once and displays a reconnect message.
- Long modpack startups require reconnecting; this gateway does not emulate a lobby or promise an indefinite login hold.
- Once ready, original handshake/login bytes and all subsequent traffic pass through unchanged. Mod-specific status fields are retained in a persistent cache.

## Safety model

The gateway and game run under separate systemd units. Restarting the gateway does not stop the game. The backend has no restart loop. RCON and the game backend must bind to loopback; only the gateway is public. A root-owned sudo rule permits the gateway user to start exactly the named backend service, with no shell or arbitrary commands.

Failed player queries are unknown, never zero. A final RCON check and an atomic join reservation protect shutdown against joining players. After `stop`, the gateway waits for the service to exit. Slow or uncertain shutdown never escalates to process signals. Startup failures do not launch a second JVM while the existing service is running. Ordinary active TCP game connections also inhibit sleep.

The backend unit's manual stop helper waits for RCON and then for the original PID to exit. Its infinite stop timeout, harmless fallback signal, and disabled SIGKILL deliberately favor preserving the world over completing an unhealthy service stop. A stuck shutdown requires operator investigation.

## Operator tasks

Run `task` in the project directory to see all commands. The project is owned by
the development account; both systemd services run as the existing Minecraft
account. Task uses `sudo -u` to run the local admin/RCON client as that account.
Java does not run as root. The daemon's sole sudo permission is to start exactly
the backend unit. Graceful automatic and guarded manual shutdown use loopback
RCON. Systemd starts Java directly, with the existing JVM/loader arguments,
instead of launching the modpack's restart loop.

| Task | Behavior |
|---|---|
| `task status` | Gateway version, Minecraft state, players, connections, health, and counters |
| `task start` | Start Minecraft once; also available as `task wake` |
| `task stop` | Save and stop only when confirmed empty; also available as `task sleep` |
| `task command -- op PlayerName` | Run a Minecraft console command and print its reply |
| `task command -- list` | Ask Minecraft for its current players |
| `task logs:server` | Recent Minecraft console output |
| `task logs:server:follow` | Follow Minecraft console output; Ctrl+C only closes the viewer |
| `task logs` / `task logs:follow` | Recent/followed gateway events |
| `task config:edit` | Edit `.env` using `$VISUAL`, `$EDITOR`, or nano |
| `task config:show` / `task config:check` | Inspect/validate proposed settings |
| `task config:apply` | Atomically save validated settings for the next gateway start |
| `task gateway:restart` | Load saved settings/new code; disconnects clients, leaves Java running |
| `task gateway:stop` / `task gateway:start` | Stop/start the gateway itself; does not directly stop/start Java |
| `task version` | Installed executable version; the running daemon's version is in `task status` |

The Minecraft console is now its systemd journal, rather than an attachable tmux
terminal. Use one terminal for `task logs:server:follow` and another for
`task command -- ...`. No RCON client installation or password entry is needed.

Commands accept normal CLI arguments. For JSON/NBT or shell metacharacters,
single-quote the whole command or the individual JSON argument:

```sh
task command -- op PlayerName
task command -- 'tellraw @a {"text":"Hello everyone"}'
task command -- tellraw @a '{"text":"Hello everyone"}'
task command -- /list
task logs:server -- --since '20 minutes ago'
```

Arguments are passed literally and joined with spaces; no second shell parses
the command. An optional leading `/` is accepted. Empty commands and line breaks
are rejected. Each invocation makes one authenticated loopback RCON connection,
prints the reply, then closes it. It does not automatically wake Minecraft or
retry a command whose reply was lost. A failed response can mean the command
already executed; inspect state before repeating a command with side effects.

`task stop` refuses occupied servers, pending/active joins, and unknown player
counts. It is a safe sleep request, not a maintenance lock: a later login wakes
the server again. If you intentionally need the console's ordinary stop command
while players are online, `task command -- stop` saves and shuts down Minecraft,
disconnecting those players. Since this bypasses the lifecycle's sleep API, the
gateway may briefly report an unexpected exit and enforce its wake cooldown.
It still does not restart Java in a loop or send kill signals.

`task gateway:stop` closes public access and suspends wake/idle monitoring. Java,
if running, remains running. Start the gateway again to restore access and
monitoring. Keep the old restart wrapper stopped while these services manage
Minecraft.

## Configuration through .env

`task config:init` creates a mode-0600 `.env` from the existing installed settings
and discovers the systemd service account. It refuses to overwrite an existing
file. `.env` is ignored by Git; `.env.example` documents available settings.

```dotenv
MC_IDLE_TIMEOUT=1h
MC_POLL_INTERVAL=15s
MC_LOG_LEVEL=info
MC_COMMAND_TIMEOUT=30s
MC_LOG_LINES=100
MC_BOOT_MESSAGE="Server is booting up.\nPlease reconnect in a few minutes."
```

Use `task config:edit`, then `task config:check` and `task config:apply`.
The apply task validates with the Go configuration checker, serializes writes,
preserves a `.previous` copy, and replaces the protected JSON atomically with
mode 0640. Invalid settings leave the installed configuration unchanged. The
RCON password remains in its protected file, not in `.env`, command arguments,
or Git.

**Applying settings does not restart either process.** The running gateway
retains its loaded configuration until you explicitly run
`task gateway:restart`. That restart disconnects any proxied players but leaves
Java and the world running. Task-only settings such as log line count and
operator command timeout take effect on the next task invocation.

Every Go configuration field has an `MC_*` setting. Omitted fields keep their
installed values. Runtime timing, messages, logging, connection limits, and
public listen address can be changed through this workflow. Backend/RCON
addresses, password-file path, backend unit, state directory, and admin socket
are tied to the installed Minecraft listeners, sudoers rule, and service
permissions. Apply rejects changes to those fields; migrating them requires a
coordinated installation change. `MC_MAX_PLAYERS` controls fallback status
display; Minecraft's actual limit remains its existing `server.properties`
setting. Changing the public listen address also requires matching your own
network/tunnel routing.

The `.env` parser treats values as data. It supports `KEY=value`, comments,
optional `export`, single-quoted literal strings, and JSON-style double-quoted
strings. JSON `\n` escapes work in player messages. It rejects unknown/duplicate
keys, multiline assignments, and invalid values. It does not expand `$variables`
or execute `$(commands)`. Do not source this file in a shell.

## Development

Requires Go 1.24+, a C compiler for the race detector, and Task.

| Task | Purpose |
|---|---|
| `task fmt` | Format source |
| `task check` | Formatting, vet, and race tests |
| `task build` | Build `bin/mc-gateway` |
| `task install VERSION=0.1.2` | First install: validate backup, install, activate, and initialize `.env` |
| `task deploy VERSION=0.1.2` | Check, build, and atomically install an update without restarting processes |
| `task fuzz:handshake` | Bounded protocol-parser fuzz run |

No external Go modules are required. Host-specific settings, credentials, caches, and binaries belong outside Git. The deployment templates use a generic `minecraft` account and server path; an installation must adapt those values to its existing server.

## Deployment

The first-install helper consumes `.local/install.json` with `user`, `server_directory`, `server_argfile` (relative to that directory), and `backup_directory`. Optional `config` overrides control messages and timing. The backup directory must contain the original `server.properties` and a tested `world-before-gateway.tar.zst` containing `world/level.dat`. It refuses an existing installation or a running Java/restart launcher. Run `task install VERSION=0.1.2`; the installer records paths, hashes, original permissions, and results in ignored `.local/installation.json`. It verifies units and configuration before activation and restores pre-activation changes if validation fails. Routine binary updates use `task deploy VERSION=...`, which retains the prior executable as `mc-gateway.previous` and leaves both services running. Restarting the gateway to use the new executable is a separate explicit task. Rerunning first-install cannot overwrite an existing deployment.

1. Confirm the server is stopped and preserve a consistent world backup and original configuration.
2. Build and test. Install the binary under `/opt/mc-gateway/bin` owned by root.
3. Adapt the two unit templates to the existing game account and directory. Set `MC_SERVER_ARGFILE` in `/etc/mc-gateway/backend.env` to the existing loader's argument file, including its leading `@`. Keep the same JVM arguments and Java runtime. The backend runs Java directly, avoiding pack restart wrappers.
4. Install the JSON configuration and a unique random RCON password in root-owned files readable only by the service group (0640). Set `server-ip=127.0.0.1`, `server-port=25566`, `enable-rcon=true`, `rcon.port=25575`, and the same password in `server.properties`. Set `broadcast-rcon-to-ops=false` so health checks do not notify players. Protect that properties file against unrelated users. Verify the actual RCON listener is loopback before exposing the gateway.
5. Add a sudoers rule for exactly `/usr/bin/systemctl start mc-gateway-backend.service` for the gateway account. Validate it with `visudo -cf`. Do not add arbitrary systemctl arguments or a shell.
6. Install the dedicated journald namespace settings. Verify units and configuration before enabling only `mc-gateway.service`. The backend must remain disabled at boot.
7. Exercise status-only requests, a waking login, readiness, genuine modded play, graceful shutdown, and a second wake. Keep the initial world backup until the real-client test passes.

## Logs and operations

Gateway INFO logs cover state transitions, actual player-count changes, proxied connection open/close with duration and byte counts, and health recovery. Normal status pings and healthy polls are silent. Error deduplication and warning throttles prevent retries from flooding the log; aggregate counters remain available through `task status`.

Health checks reuse one authenticated loopback RCON connection, avoiding Minecraft's per-connection log chatter. A broken or uncertain exchange discards that connection and fails the check; a later poll reconnects. Commands with uncertain delivery are never automatically replayed.

The dedicated `mc-gateway` journald namespace has a 64 MiB persistent budget, 8 MiB file rotation, a 16 MiB runtime budget, and a 14-day retention limit. Gateway and backend events can be inspected separately by unit. The game's existing own log files retain their existing behavior.

The admin API is a mode-0600 Unix socket, not a public HTTP listener. It supports status, wake, and guarded sleep only. It cannot execute arbitrary RCON commands. `task sleep` refuses occupied or unhealthy servers; it is not a force-stop command.

Use the operator tasks above to run the installed server. Keep the original restart wrapper for rollback; running it alongside the gateway would create an unmanaged Java process outside the lifecycle controller.

## Compatibility boundaries

- The initial envelope is validated for the tested Minecraft version; encrypted/compressed/gameplay/mod packets remain opaque.
- Backend source addresses become loopback. Existing IP bans or per-IP limits will see the proxy address. No unsupported PROXY headers are injected.
- Mods using separate UDP/TCP ports need their own routing review.
- Existing authentication, whitelist, mods, JVM tuning, and world content are not changed by the gateway.
- Fake players reported by RCON may deliberately keep the server awake; unknown responses always inhibit sleep.
- Configured timeouts report stalled boot/save operations; they never kill Java.

## Rollback

When empty, use `task sleep` and wait for `asleep`. Stop and disable only the gateway service, confirm Java is gone, restore the preserved `server.properties` and its original permissions, and start Minecraft using its original launcher. Do not restore an older world merely to remove the gateway; that would discard subsequent gameplay. Keep host-specific backup locations and the precise installation record in ignored `.local/` files.
