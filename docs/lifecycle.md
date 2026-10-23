# Lifecycle and access

## What a player sees

A server-list ping gets a status response without starting Minecraft. An allowed login to a sleeping server requests one start and gets `MC_BOOT_MESSAGE`. During startup, reconnect attempts show the remaining estimate learned from previous successful boots. Once Minecraft is ready, reconnecting joins normally; handshake bytes and subsequent modded traffic pass through unchanged.

The gateway does not hold a modded client in a fake lobby. The retry message is a disconnect screen and requires reconnecting. Admission follows Minecraft's native whitelist when enabled.

## Sleep and stop

After `MC_IDLE_TIMEOUT` (default **15m**) of confirmed emptiness, the controller sends loopback RCON `stop` so Minecraft saves and exits. Unknown/failed player checks, active proxy connections and pending joins prevent idle shutdown. Wake/sleep operations are serialized, and the backend service has `Restart=no`, so a deliberate stop cannot launch it again automatically.

`start`/`wake` requests a start. `stop`/`sleep` requests guarded shutdown and refuses players or active joins. A later login can wake it again. A slow save is allowed to finish; neither the controller nor systemd force-kills Java. Startup timeouts report failure without killing the process, and a later successful health check can recover it.

`gateway stop` disables public access and wake monitoring while Minecraft keeps running. `gateway restart` preflights configuration, sends the configured player disconnect message where possible, replaces the gateway and adopts the existing backend. True server uptime continues across this restart; startup history is not fabricated from adoption.

## Which account runs what?

Both gateway and Minecraft run as `MC_SERVICE_USER`, the non-root account that owns the server. The operator CLI uses noninteractive sudo for private socket and service operations. The daemon's sudoers rule permits only starting its configured backend unit; it does not give the daemon general root access.

Systemd runs the backend launch helper, which enters `MC_SERVER_DIRECTORY` and executes `MC_LAUNCH_EXECUTABLE` with literal `MC_LAUNCH_ARGS`. The backend and RCON listen on loopback. The private Unix control socket belongs to the service account. All operator settings and the password come from the canonical `.env`.

Minecraft output goes to a journald namespace. Use `logs --server --follow` to watch it and `command -- <command>` to enter operator commands. There is no tmux attachment to find.

## Quick diagnosis

| Symptom | Check |
| --- | --- |
| Minecraft stays asleep | `status`, `logs`, native whitelist, then `start`; status pings intentionally do not wake it. |
| Boot is taking longer than estimated | `logs --server --follow`; estimates are approximate and never force an early stop. |
| Server does not sleep | `status` for players, active connections or unknown health; `stats` for RCON availability. |
| Config edit has no effect | `config check`, `config reload`, then compare `config show` with `config live`. Restart-only changes are rejected explicitly. |
| Gateway socket unavailable | `gateway start`; use `logs` for service errors. `command --direct` can reach a still-running backend's loopback RCON. |
| Missing JVM metrics | Ensure the foreground launcher `exec`s Java and the service account can inspect that process; see [Metrics](metrics.md). |
| Folder sizes are old | Check scan success and age in `metrics`; incomplete scans retain marked last-good totals. |

Routine polls and status pings do not generate log spam. State transitions and actionable failures are logged; `--json` gives structured output. Boot-cache problems affect estimates, not Minecraft's world or lifecycle safety.
