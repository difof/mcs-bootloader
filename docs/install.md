# Install, update and remove

## Requirements

- An existing working [supported Minecraft Java server](compatibility.md) and the Java/runtime required by its modpack.
- Linux with systemd, journald namespaces, `systemctl`, `systemd-analyze`, `journalctl` and `busctl`.
- Python **3.10+**, `sudo`/`visudo`, `runuser`, `getconf`, and standard core utilities. `sha256sum` is useful for checking the downloaded release, not a runtime dependency. No archive/compression tools are required.
- An existing non-root account owning the server and an operator with sudo access. Run `sudo -v` if you need to authenticate; subsequent tool operations use noninteractive sudo.

One release executable contains the operator helpers, templates and default configuration. It can run from an otherwise empty directory. The bundled installer manages one instance under `/opt/mcs-bootloader`, `/etc/mcs-bootloader`, `/run/mcs-bootloader` and `/var/lib/mcs-bootloader`.

## First installation

1. Download the matching `linux_amd64` or `linux_arm64` executable and `SHA256SUMS` from [Releases](https://github.com/difof/mcs-bootloader/releases). In the download directory, run `sha256sum --check --ignore-missing SHA256SUMS`, then rename the executable to `mcs-bootloader`.
2. Stop Minecraft and its old launcher, tmux restart loop or service. Keep them stopped through installation.
3. Initialize and edit the configuration:

```sh
chmod +x ./mcs-bootloader
./mcs-bootloader init
./mcs-bootloader config edit
```

`init [PATH]` creates a private file with a random RCON password and refuses to overwrite one. Set at least:

```dotenv
MC_SERVICE_USER=minecraft
MC_SERVER_DIRECTORY=/srv/minecraft
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["-Xms2G","-Xmx6G","-jar","server.jar","nogui"]'
MC_PROTOCOL=767
MC_VERSION=1.21.1
MC_IDLE_TIMEOUT=15m
```

Match the protocol/version to your backend; see the [compatibility table](compatibility.md#supported-versions).

Choose your public/backend/RCON addresses before installation. The defaults are `:25565`, `127.0.0.1:25566` and `127.0.0.1:25575`; backend and RCON must use the same numeric loopback host and different ports.

```sh
./mcs-bootloader install
./mcs-bootloader status
```

Installation checks the stopped server/launcher, free ports, service account, configuration and ownership. It updates only the required network/RCON properties, preserves worlds/mods/authentication/whitelist settings, and starts the gateway. It neither creates nor requires a server backup. Pre-activation failures roll back this installation's changes; if service activation has begun, a failure keeps the validated files for inspection rather than changing files under a possibly starting server.

The canonical configuration becomes `/etc/mcs-bootloader/.env`; the original path becomes a symlink to it. A private installation report is written beside the original file, or at `install --report PATH`. The report contains no password.

Forward only the gateway's public port. Minecraft starts on the first allowed login or `start`. The installed executable is `/opt/mcs-bootloader/bin/mcs-bootloader`; optionally symlink it into your PATH. The guides assume it is on your PATH.

## Launching a modpack

Use the actual argument-file paths from the pack. For Forge/NeoForge:

```dotenv
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["@user_jvm_args.txt","@libraries/net/neoforged/neoforge/YOUR_VERSION/unix_args.txt","nogui"]'
```

A custom script works too:

```dotenv
MC_LAUNCH_EXECUTABLE=/bin/bash
MC_LAUNCH_ARGS='["./start-once.sh","--your-option","value with spaces"]'
```

The working directory is `MC_SERVER_DIRECTORY`. Relative executable paths resolve there; arguments are literal JSON strings, with no shell expansion. A script must run once in the foreground, preferably `exec` Java so process metrics can identify it. Remove automatic restart loops from the chosen launch path.

## Update an installation

Download and verify the new executable, then run that downloaded file:

```sh
chmod +x ./mcs-bootloader
./mcs-bootloader --env /etc/mcs-bootloader/.env deploy
/opt/mcs-bootloader/bin/mcs-bootloader gateway restart
/opt/mcs-bootloader/bin/mcs-bootloader status
```

`deploy` validates and atomically replaces the installed executable, retaining the previous executable for rollback. It does not restart either process. The explicit gateway restart applies the new code and disconnects players with the configured restart reason where RCON is available; Java continues running. Review new/removed `.env` keys before deployment. For v0.5.0, remove the retired `MC_BACKUP_DIRECTORY` key; existing external archives remain untouched.

Source users run `task check`, then `task deploy` and `task gateway:restart`. Use `gateway configure` to refresh the tool-owned service drop-in without restarting processes.

## Remove

```sh
mcs-bootloader stop
mcs-bootloader status             # Wait for state=asleep after saving finishes.
mcs-bootloader remove --dry-run
mcs-bootloader remove --yes
```

Removal identifies the installation's files and refuses active/unknown Minecraft processes or unrecognized service overrides. It removes owned service/executable files and preserves the server directory, world/mods, external data, private `.env` and state. It does not recursively delete configurable paths or rewrite Minecraft networking. Before returning to a standalone launcher, deliberately adjust its loopback `server-ip`/`server-port` to the networking you want.
