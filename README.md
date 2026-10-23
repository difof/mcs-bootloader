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
account. Task uses `sudo -u` to run the local socket client as that account.
Java does not run as root. The daemon's sole sudo permission is to start exactly
the backend unit. Graceful automatic and guarded manual shutdown use loopback
RCON. Systemd runs a small startup helper that obtains the daemon's applied
configuration through the private socket and then executes Java with the
existing JVM/loader arguments. There is no modpack restart loop.

| Task | Behavior |
|---|---|
| `task status` | Gateway version, Minecraft state, players, connections, health, counters, and applied configuration generation |
| `task start` | Start Minecraft once; also available as `task wake` |
| `task stop` | Save and stop only when confirmed empty; also available as `task sleep` |
| `task command -- op PlayerName` | Run a Minecraft console command and print its reply |
| `task command -- list` | Ask Minecraft for its current players |
| `task logs:server` | Recent Minecraft console output |
| `task logs:server:follow` | Follow Minecraft console output; Ctrl+C only closes the viewer |
| `task logs` / `task logs:follow` | Recent/followed gateway events |
| `task config:edit` | Edit the canonical `.env` using `$VISUAL`, `$EDITOR`, or nano |
| `task config:show` | Show the desired file settings and defaults, with the password redacted |
| `task config:live` | Show the daemon's applied settings and generation, with the password redacted |
| `task config:check` | Validate `.env` and check that the service account can read it |
| `task config:apply` / `task reload` | Apply supported changes through the socket, without restarting either service |
| `task gateway:restart` | Load new code or startup-only settings; disconnects clients, leaves Java running |
| `task gateway:stop` / `task gateway:start` | Stop/start the gateway itself; does not directly stop/start Java |
| `task version` | Installed executable version; the running daemon's version is in `task status` |

Minecraft console output is in its systemd journal. Use one terminal for
`task logs:server:follow` and another for `task command -- ...`. No RCON client
installation or password entry is needed. Console commands travel over the
private Unix socket; the daemon supplies its applied RCON credentials.

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
are rejected. The daemon serializes the request with its authenticated loopback
RCON connection and returns the reply. It does not automatically wake Minecraft
or retry a command whose reply was lost. A failed response can mean the command
already executed; inspect state before repeating a command with side effects.

The command task reads the invoking Task process's original arguments through
Linux `/proc`, avoiding Task 3.44's recursive substitution inside its CLI argument
variables. This preserves literal `{{...}}` text when Task starts the command.
Task can still reject some template expressions during its own initialization;
such an error prevents the command from reaching Minecraft.

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

The project `.env` is a symlink to **`/etc/mc-gateway/.env`**. That single file
holds the gateway, Java startup, operator settings, and actual
`MC_RCON_PASSWORD`. There is no active JSON configuration, separate RCON
password file, or `backend.env`. `.env` is ignored by Git; `.env.example`
documents every supported setting.

The canonical file belongs to the operator, with the Minecraft service group
and mode **0640**. Its directory has the same ownership and mode **2750**. The owner can edit it and the
service account can read it; unrelated users cannot traverse that directory.
The directory's setgid bit keeps the service group on replacement files.
`task config:edit` resolves the project symlink before starting the editor and
sets umask `0027`, so ordinary atomic editor saves keep the one-file layout.
The private development home directory does not need to become accessible to
the service account.

`task config:init` restores the project symlink to an existing canonical file.
It refuses to overwrite a local copy or a different link. If an external editor
has replaced the symlink with a separate `.env`, tasks reject that split rather
than applying a hidden second configuration; move the duplicate aside and run
`task config:init` after reviewing its edits.

```dotenv
MC_IDLE_TIMEOUT=1h
MC_POLL_INTERVAL=15s
MC_LOG_LEVEL=info
MC_COMMAND_TIMEOUT=30s
MC_LOG_LINES=100
MC_BOOT_MESSAGE="Server is booting up.\nPlease reconnect in a few minutes."
```

Use the following workflow:

```sh
task config:edit
task config:check
task config:apply       # task reload is the same operation
task config:live
```

Editing saves the desired configuration on disk. `config:check` checks syntax,
runtime constraints, the credential, and access as the service account; it does
not apply anything or decide whether a particular change is safe to load live.
`config:apply` sends a fingerprint of that file to the running daemon through
`/run/mc-gateway/control.sock`. The daemon reads its own canonical file,
validates the complete candidate, and serializes the change with wake/sleep.
It rejects a stale fingerprint, an invalid configuration, or a prohibited live
change before changing its applied state. Correct the file and apply again.

**A successful live reload preserves the listener and existing game connections
without restarting Java.** The daemon reports its configuration generation and
changed field names, and emits one useful configuration event rather than a
settings dump. `task config:show` describes the file; `task config:live` describes the
running daemon. Both redact the password. `task status` includes the applied
generation and file fingerprint, so an unapplied edit cannot masquerade as a
running change. Generation starts fresh when the daemon restarts.

| Settings | When changes take effect |
|---|---|
| Idle timeout, poll interval, operation/startup timeouts, shutdown warning, wake cooldown | Live after `task config:apply`; shortening idle timeout can allow sleep on the next verified-empty poll |
| Messages/MOTDs and log level | Live after apply; subsequent responses and log events use the new settings |
| `MC_VERSION`, `MC_MAX_PLAYERS` | Live fallback status metadata; cached real-server metadata and Minecraft's native player limit remain authoritative |
| Connection limit, handshake timeout, backend dial timeout | Apply affects new connections; existing opaque relays stay open even if the new limit is lower |
| `MC_RCON_PASSWORD` | Apply only while Minecraft is confirmed stopped; see password rotation below |
| Listen/backend/RCON addresses, protocol, backend service, state directory, socket | Live apply rejects these; require a gateway restart and any matching listener, unit, permissions, or tunnel migration |
| `MC_SERVER_DIRECTORY`, `MC_JAVA_BINARY`, `MC_JVM_ARGFILE`, `MC_SERVER_ARGFILE` | Live apply rejects these; load with an explicit gateway restart, then Java uses them on its next start |
| Task-only settings such as `MC_LOG_LINES` and `MC_COMMAND_TIMEOUT` | Next task invocation reads them directly from `.env` |

The backend startup helper obtains the **applied** startup settings and password
from the daemon's private socket. Editing `.env` without applying or explicitly
restarting the gateway therefore cannot silently change the next Java launch.
Gateway restart disconnects proxied clients and adopts any already-running
Minecraft process; it does not restart Java or change that process's arguments.
Before stopping a running gateway, `task gateway:restart` asks it to validate
the candidate configuration. The preflight rejects **any** password difference
from the applied credential, even when Minecraft is stopped, and leaves the
old gateway running. Apply a password change through `task config:apply` while
Minecraft is stopped before requesting a restart. Gateway restart therefore
cannot bypass the password rotation guard.

Omitted runtime fields use compiled defaults, not values from an older file.
Keep the installation's host-specific paths and service settings in `.env`.
`MC_MAX_PLAYERS` controls fallback status display; Minecraft's actual player
limit remains in its existing `server.properties`.

### Password rotation

Stop Minecraft gracefully, wait until `task status` confirms `asleep`, then
change `MC_RCON_PASSWORD` with `task config:edit`, check, and apply. The daemon
refuses a password change while Minecraft is running or its state is uncertain.
It retains the previous applied credential when rejecting the change. A login
can still wake a sleeping server, so an attempted rotation that races a wake
is rejected safely and must be retried while stopped.

Minecraft itself requires `rcon.password` in its native `server.properties`.
The backend helper synchronizes that one property from the applied `.env`
credential before the next Java start, preserving unrelated settings. This is
Minecraft's required input, not another operator-maintained gateway config.
Changing `.env` alone does not rewrite a running server's properties or attempt
to change its in-memory password. Historical rollback archives may retain old
configuration and credentials; they are never active runtime inputs.

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
| `task check` | Formatting, vet, Go race tests, and Python operator/install tests |
| `task build` | Build `bin/mc-gateway` |
| `task install VERSION=0.2.0` | First install from `.env`: validate backup, install, activate, and link the canonical file |
| `task migrate:env` | One-time migration from the earlier JSON/password-file installation; restarts the gateway once |
| `task deploy VERSION=0.2.0` | Check, build, and atomically install an update without restarting processes |
| `task fuzz:handshake` | Bounded protocol-parser fuzz run |

No external Go modules are required. Host-specific settings, credentials, caches, and binaries belong outside Git. The deployment templates use a generic `minecraft` account and server path; an installation must adapt those values to its existing server.

## Deployment

The first-install helper reads `.env` using `scripts/install.py --env .env`.
Copy `.env.example` into a private `.env` and set the existing service account,
server directory, Java binary, JVM and loader argument files, backup directory,
and a strong private RCON password. Argument-file values include their leading
`@` and remain relative to the server directory. Use the modpack's actual
existing files rather than guessing a loader version.

The backup directory must contain the original `server.properties` and a tested
`world-before-gateway.tar.zst` containing `world/level.dat`. The installer refuses
an existing installation or a running Java/restart launcher. Run
`task install VERSION=0.2.0`; it validates the units and configuration before
activation, installs the canonical `.env`, and links the project file to it.
It records installation evidence in ignored `.local/installation.json`, and
restores pre-activation changes if validation fails. That report is an audit
record, not a second configuration source.

For an existing installation, run `task check` and `task build VERSION=0.2.0`,
then `task migrate:env` once. The self-elevating migration consolidates the prior
configuration and password into the canonical file, installs the new binary and
unit helpers, validates everything, and **automatically restarts only the
gateway once**. That restart disconnects proxied clients. The helper verifies
the new daemon's status and that the Minecraft PID is unchanged; it does not
restart Minecraft or edit world/mod data. No additional gateway restart is
needed after a successful migration.

Obsolete inputs are retired into a root-owned mode-0600 historical archive at
`.local/env-migration-TIMESTAMP.tar.gz`, with a separate redacted report. The
archive is rollback evidence and is never read for active configuration.
Routine binary updates use `task deploy VERSION=...`, which validates and
atomically installs the exact tested executable, retains
`mc-gateway.previous`, and leaves both services running. Activate the new
executable with `task gateway:restart` when ready.

1. Confirm the server is stopped and preserve a consistent world backup and original configuration.
2. Build and test. Install the binary under `/opt/mc-gateway/bin` owned by root.
3. Adapt the two unit templates to the existing game account. Put startup settings and the credential in the one canonical `.env`; keep the existing JVM arguments and Java runtime. Backend startup obtains an applied snapshot through the socket and executes Java directly.
4. For the first installation, set `server-ip=127.0.0.1`, `server-port=25566`, `enable-rcon=true`, `rcon.port=25575`, and the applied password in `server.properties`. Set `broadcast-rcon-to-ops=false` so health checks do not notify players. Protect that native properties file against unrelated users. Verify the actual RCON listener is loopback before exposing the gateway.
5. Add a sudoers rule for exactly `/usr/bin/systemctl start mc-gateway-backend.service` for the gateway account. Validate it with `visudo -cf`. Do not add arbitrary systemctl arguments or a shell.
6. Install the dedicated journald namespace settings. Verify units and configuration before enabling only `mc-gateway.service`. The backend must remain disabled at boot.
7. Exercise status-only requests, a waking login, readiness, genuine modded play, graceful shutdown, and a second wake. Keep the initial world backup until the real-client test passes.

## Logs and operations

Gateway INFO logs cover state transitions, actual player-count changes, proxied connection open/close with duration and byte counts, and health recovery. Normal status pings and healthy polls are silent. Error deduplication and warning throttles prevent retries from flooding the log; aggregate counters remain available through `task status`.

Health checks reuse one authenticated loopback RCON connection, avoiding Minecraft's per-connection log chatter. A broken or uncertain exchange discards that connection and fails the check; a later poll reconnects. Commands with uncertain delivery are never automatically replayed.

The dedicated `mc-gateway` journald namespace has a 64 MiB persistent budget, 8 MiB file rotation, a 16 MiB runtime budget, and a 14-day retention limit. Gateway and backend events can be inspected separately by unit. The game's existing own log files retain their existing behavior.

The admin API is a mode-0600 Unix socket owned by the service account. It supports
status, redacted applied configuration, validated reload, wake, guarded sleep,
and operator console commands. A separate internal endpoint supplies the applied
startup snapshot to the backend helper; that response includes the credential
and is restricted by the same private socket permissions. There is no public
HTTP admin listener. `task sleep` refuses occupied or unhealthy servers; it is
not a force-stop command.

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
