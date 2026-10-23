# Working agreements

- Use Go's standard library unless a dependency clearly reduces risk.
- Operate through Taskfile tasks. Keep changes focused and reviewable.
- Preserve original Minecraft handshake/login bytes and opaque mod traffic.
- Treat unknown player counts as occupied. Never force-kill the game server.
- Serialize wake and sleep; protect pending joins against idle shutdown.
- Keep configuration, credentials, caches, logs, binaries, and host details out of Git.
- Log state transitions and actionable errors; do not log routine polling or status pings.
- Run formatting, vet, race tests, and protocol/lifecycle integration tests before deployment.
- Never change a live world, authentication settings, mods, or unrelated services without authorization.
- Read native Minecraft whitelist policy and entries; never introduce a separate gateway whitelist toggle or edit the native files to test a feature.
- Collect metrics without waking Minecraft. HTTP scrapes must use cached server samples, bounded labels, and no command text, passwords, JVM arguments, or raw RCON replies.

## Configuration and operator interface

- Keep user-facing operations available through Task. Treat CLI arguments and `.env` values as literal data, never shell code or Task templates.
- `.env` is the single operator-controlled configuration source, including the RCON password. Socket reloads read an explicit resolved path and validate the snapshot; they do not write settings or create a second active config file.
- When adding, renaming, or removing a setting, update Go parsing/defaults/validation, Python schema/defaults/validation, `.env.example`, Task usage, and relevant documentation together. Keep the example complete and safe, and test schema/default parity.
- During a deployment, bring the installed private `.env` up to date with the same keys while preserving existing operator values, ownership, permissions, and credentials. Never copy the live file into Git, test output, release notes, or packages.
- Make tunable behavior configurable with explicit units and safe defaults. Protocol bounds and defensive implementation limits should remain documented safety limits, not dozens of unnecessary settings.
- Apply supported live settings atomically. Bind/validation failures must preserve the previous configuration and listener. Startup-only changes must explain the required restart or migration.

## Releases and Actions budget

- Ordinary fixes, commits, pushes, deployments, and version-string changes are **not permission to release**. Never create or push a release tag, or publish a release, unless the owner explicitly requests that release.
- Batch useful changes into intentional SemVer releases. Stable tags use `vMAJOR.MINOR.PATCH`, for example `v0.4.0`; no leading zeroes. Do not create a tag for each edit or failed workflow attempt unless the owner explicitly authorized corrective release tags for that attempt.
- Before an approved tag: review the diff, run `task check`, and build/inspect `task release:package VERSION=vX.Y.Z`, including its generated `RELEASE_NOTES.md`. Packaging is local and never creates tags, pushes, or releases.
- Published tags and assets are immutable by policy. Never move/delete/recreate a published tag or overwrite its release assets. Fixes normally belong in a later, separately approved version. Recover an interrupted unpublished draft deliberately; do not silently replace it.
- Keep the workflow tag-only: no branch/PR builds, schedules, automatic version bumps, or automatic tagging. One standard Linux runner, one test pass, two cross-compiled Linux packages, and a 10-minute job limit. No build matrix, containers, or Actions artifact uploads/caches without an explicit budget decision.
- Write useful commit subjects: release packaging generates the **Changelog** from Git history since the previous reachable stable release tag. Do not maintain `CHANGELOG.md` or manually invent release highlights. Keep release pages free of Downloads and Install or update sections; installation instructions belong in README.
- Keep actions pinned to reviewed commit SHAs. Include installation files, checksums, version/commit/toolchain metadata, and generated change notes. Exclude real `.env`, server/world data, logs, backups, and machine-specific state from every package.
