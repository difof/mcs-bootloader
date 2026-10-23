# Changelog

Changes are collected here until the owner explicitly requests a release. There
are no published versions of mcs-bootloader yet. Release tags use `vMAJOR.MINOR.PATCH`.

## Unreleased

### Added

- Wake Minecraft on a login attempt and stop it gracefully after an hour with no players.
- Pass through original handshake/login bytes and opaque mod traffic while the server is ready.
- Show a sleeping/starting status and a reconnect message during cold startup.
- Private local RCON, a protected control socket, live configuration reload, and console commands through Task.
- Configurable server directory, launch executable, and literal JSON argument array in one private `.env`.
- Linux amd64/arm64 release bundles with installation helpers, source, checksums, and build metadata.

### Changed

- Rename the project, binary, service names, and standard install paths to `mcs-bootloader`.
- Keep routine polling quiet and record state changes and actionable failures.

### Upgrade notes

- This is the first packaged release of the renamed project. Existing experimental `mc-gateway` installations need a planned service/configuration migration; the first-install task intentionally refuses to overwrite an installation.
- Launch configuration now uses `MC_LAUNCH_EXECUTABLE` and `MC_LAUNCH_ARGS` instead of Java-specific argument-file settings.
- Supported protocol: Minecraft Java 1.21.1 (767), on Linux with systemd. A cold join receives a reconnect message; it does not hold the client in a fake game world.
