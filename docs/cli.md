# CLI and Task reference

Use `mcs-bootloader help`, `help <command>`, or `<command> --help`. The executable works without a source checkout. Relative `.env` paths resolve from your current directory.

## Output and common flags

| Flag | Behavior |
| --- | --- |
| `--env PATH` / `--config PATH` | Select `.env`; default is local `./.env`, then `/etc/mcs-bootloader/.env`. |
| `--json` | JSON results and errors; NDJSON for log streams. No ANSI escapes. Errors use stderr and a nonzero exit code. |
| `--color auto\|always\|never` | Human output colors; `auto` follows the terminal and `NO_COLOR`. |
| `--socket PATH` | Select a private administration socket for recovery/automation. |
| `--help` / `-h` | Show help without performing the operation. |

The password is redacted in all configuration output. Put CLI flags before the first Minecraft command word, or use `--` to separate literal command data.

## Commands

| Command | Purpose |
| --- | --- |
| `init [PATH]` / `config new [PATH]` | Create a private `.env` with a random password; refuse overwrite. |
| `config link [PATH]` | Link a local file path to the canonical installed `.env`. `config init` is an alias. |
| `config edit` | Edit the resolved file using `VISUAL`, `EDITOR`, then `nano`. |
| `config show` / `config live` | Inspect desired disk settings / applied daemon settings. |
| `config check` | Validate values, permissions and service-account read access. |
| `config reload [PATH]` | Atomically re-read the canonical file over the socket. `reload` is an alias. |
| `install [--report PATH]` | First installation against a stopped Minecraft server/launcher. |
| `deploy` | Install this executable as an update without restarting processes. |
| `remove [--dry-run\|--yes]` | Preview removal by default; `--yes` removes identified installation files. |
| `status` | State, player/connection counts, configuration generation and running version. |
| `stats` | Compact cached telemetry, uptime, boot estimates and folder sizes. |
| `start` / `wake` | Request one start; return while Minecraft boots. |
| `stop` / `sleep` | Save and stop only when empty, with known player state and no active joins. |
| `command [--timeout 30s] [--direct] -- COMMAND ...` | Send one literal console command; never wake Minecraft or replay it. |
| `gateway start\|stop\|restart\|configure` | Manage public access, monitoring and the owned service settings. Java stays running across gateway stop/restart. |
| `logs [--server] [--follow] [--lines N]` | Gateway events or Minecraft console output; Ctrl+C exits the viewer. |
| `metrics [--raw]` | Cached metric table; `--raw` prints Prometheus text, `--json` gives structured samples. |
| `serve` | Run the gateway in the foreground; normally systemd owns this process. |
| `version` / `help` | Executable version / command help. `status` reports the running daemon version. |

## Minecraft console

```sh
mcs-bootloader command -- op PlayerName
mcs-bootloader command -- whitelist add PlayerName
mcs-bootloader command -- whitelist reload
mcs-bootloader command -- 'tellraw @a {"text":"Hello!"}'
mcs-bootloader logs --server --follow
```

Commands go through the gateway's private socket to loopback RCON. The CLI preserves quotes and special characters as data; it does not execute a shell. `--direct` connects to the configured loopback RCON endpoint when the gateway is unavailable. It still requires a running Minecraft server. Prefer the guarded `stop` command for shutdown; an explicit console `stop` is an operator command to Minecraft itself.

## Source development with Task

Use Go 1.24+, Python 3.10+, Task 3, and a C compiler for race tests. Run from the repository root. Every operator wrapper runs `go build` first, reusing Go's cache and embedding the current scripts/templates. No separate manual build is required. Arguments after Task's `--` are passed literally through the binary's Task adapter, including spaces, `$`, quotes and `{{...}}`.

| Task | Binary command |
| --- | --- |
| `task init`, `task config:new` | `init`, `config new` |
| `task config:link` (alias `config:init`) | `config link` |
| `task config:edit`, `config:show`, `config:live`, `config:check` | Corresponding `config` subcommand |
| `task config:reload -- PATH` (aliases `reload`, `config:apply`) | `config reload PATH` |
| `task install`, `task deploy` (aliases `install:binary`, `deploy:binary`) | Build and call `install` / `deploy`; run `task check` first when deploying source changes |
| `task remove -- --dry-run` | `remove --dry-run` |
| `task status`, `stats`, `metrics`, `start`, `stop` | Same command; `wake` and `sleep` aliases also work |
| `task command -- op PlayerName` | `command -- op PlayerName` |
| `task logs`, `logs:follow`, `logs:server`, `logs:server:follow` | `logs` with the corresponding flags |
| `task gateway:start`, `gateway:stop`, `gateway:restart`, `gateway:configure` | Corresponding `gateway` subcommand |
| `task help`, `serve`, `version` | Same command |
| `task config -- show`, `task gateway -- restart` | Command-group access |
| `task cli -- <arguments>` | Any CLI operation, flags or aliases |

Append output flags after the separator: `task status -- --json` or `task metrics -- --raw`.

The systemd/compatibility helpers are also reachable: `task backend-start`, `task backend-stop -- --pid PID`, `task restart-check`, and `task config:validate` (binary `check`). Backend helpers refuse execution as root or outside their configured systemd service. They exist for service integration; everyday server control uses `start`/`stop` and `gateway:*`.

Development tasks are `build`, `fmt`, `fmt:check`, `vet`, `test`, `check`, and `fuzz:handshake`. `task check` runs formatting, vet, race tests and Python tests. `task release:package VERSION=vX.Y.Z` checks and builds local standalone assets; it never tags or publishes. Release Changelog entries are generated from Git commit subjects/links since the highest earlier reachable stable tag. See [AGENTS.md](../AGENTS.md) for the deliberate-release and Actions budget policy.
