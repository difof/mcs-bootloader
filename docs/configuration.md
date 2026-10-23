# Configuration

The private `.env` is the single operator-controlled source, including `MC_RCON_PASSWORD`. [`.env.example`](../.env.example) lists every setting with safe defaults. Initialize using `mcs-bootloader init`; do not use the example password for an installation.

## Edit and apply

```sh
mcs-bootloader config edit
mcs-bootloader config check
mcs-bootloader config reload /etc/mcs-bootloader/.env
mcs-bootloader config live
```

Commands select explicit `--env PATH`, otherwise local `.env`, otherwise `/etc/mcs-bootloader/.env`. Installation links the original path to that canonical file. Reload resolves the requested path and sends its digest through the private Unix socket; the daemon validates exactly those bytes and commits the complete configuration atomically. A changed file, invalid value or occupied metrics port rejects the reload and preserves the applied settings. Reload never writes `.env` and accepts only the daemon's canonical source or a symlink to it.

Files are data: `KEY=value`, comments, single-quoted literals and JSON double-quoted strings. `$variables`, `$(commands)` and Task templates are not expanded. Keep one assignment per physical line; player-facing messages may use JSON `\n` escapes. Private permissions are `0600` or installed `0640`, with read access for the service account. `config show` and `config live` redact the password.

| Change | Application |
| --- | --- |
| Timers, messages, MOTDs, advertised version, limits, logging, all metrics settings | `config reload` |
| Server directory, launch executable/arguments, protocol number | Gateway restart; launch changes affect the next Minecraft start |
| RCON password | Stop Minecraft, wait until asleep, then reload; password changes are refused while it is running/unknown |
| Listener/backend/RCON addresses, service account/units, socket/state paths, log namespace | Coordinated service/network migration; these cannot be changed by live reload |

The default `MC_IDLE_TIMEOUT=15m` means 15 minutes of confirmed emptiness. Keep `MC_POLL_INTERVAL` at or below the idle timeout. Durations use explicit units (`15m`, `30s`, `500ms`).

## Startup estimate

```dotenv
MC_BOOT_MESSAGE="Server is booting up. {eta} Please reconnect."
# Optional elapsed time:
# MC_BOOT_MESSAGE="Server is booting up ({elapsed} elapsed). {eta} Please reconnect."
```

`{eta}` expands to a sentence containing the approximate remaining time, or an honest unknown/overdue message. `{elapsed}` is the elapsed time for the current startup attempt, or `unknown` when adopting a process already starting. Each new connection sees the estimate minus time already spent booting; repeated joins do not reset the timer. The disconnect screen is a snapshot, so reconnect to get a newer estimate or enter the ready server.

The gateway learns only after observing its own complete successful startup. Adopting an already-running Java process, a failed boot or a gateway restart does not create a misleading sample. A small private `boot-history.json` under `MC_STATE_DIRECTORY` survives gateway restarts. It is runtime state, not a second configuration file, and contains no server files or credentials. The first unmeasured boot has no invented ETA. A static custom message stays static unless you add the placeholders.

## MOTD and restart message

```dotenv
MC_OVERRIDE_MOTD=true
MC_MOTD="Our modded server\nJoin to wake it up"
MC_RESTART_MESSAGE="Gateway is restarting. Please reconnect in a moment."
MC_RESTART_GRACE=3s
```

The override also applies while Minecraft runs and preserves other status/mod metadata. Without it, the running server's native MOTD is used; the sleeping/starting/stopping/unknown/failed messages are individually configurable. A controlled gateway stop/restart asks Minecraft to deliver `MC_RESTART_MESSAGE` before closing relays. Crashes or unavailable RCON cannot guarantee the message.

## Native whitelist

There is no gateway whitelist toggle. It reads `white-list` from `server.properties` and entries from `whitelist.json`. Use the normal in-game commands or:

```sh
mcs-bootloader command -- whitelist add PlayerName
mcs-bootloader command -- whitelist reload
mcs-bootloader command -- whitelist on
```

Native changes are picked up on the next login/metrics poll. If enabled but unreadable/malformed, the gateway rejects admission without waking Minecraft. The early check uses the claimed login name; Minecraft performs final authentication and admission.

## Metrics and folder sizes

Set `MC_METRICS_ENABLED=true` and `MC_METRICS_LISTEN=127.0.0.1:9464`, then reload. Named absolute paths in `MC_METRICS_DIRECTORIES` enable background folder measurements. All settings, collection limits, privacy options, uptime and startup metrics are documented in [Metrics](metrics.md).
