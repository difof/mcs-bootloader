# Working agreements

- Use Go's standard library unless a dependency clearly reduces risk.
- The standalone binary is the operator interface. Source Taskfile tasks are thin wrappers around the built binary or development tools. Keep changes focused and reviewable.
- Preserve original Minecraft handshake/login bytes and opaque mod traffic.
- Treat unknown player counts as occupied. Never force-kill the game server.
- Serialize wake and sleep; protect pending joins against idle shutdown.
- Keep configuration, credentials, caches, logs, binaries, and host details out of Git.
- Log state transitions and actionable errors; do not log routine polling or status pings.
- Run formatting, vet, race tests, and protocol/lifecycle integration tests before deployment.
- Never change a live world, authentication settings, mods, or unrelated services without authorization.
- Read native Minecraft whitelist policy and entries; never introduce a separate gateway whitelist toggle or edit the native files to test a feature.
- Collect metrics without waking Minecraft. HTTP scrapes must use cached server samples, bounded labels, and no command text, passwords, JVM arguments, or raw RCON replies.
- Directory-size collection is read-only and runs separately from RCON/lifecycle polling. Never follow symlinks or read file contents/devices. Bound concurrency and traversal, retain explicit health/freshness for incomplete scans, and never wait on disk during a scrape or shutdown.

## Configuration and operator interface

- Every operator operation must work from a downloaded executable in an empty directory, without Go, Task, Git, or a source checkout. Embed an explicit allowlist of operational Python scripts, service templates and the safe `.env.example`; do not embed whole directories or duplicate generated copies of assets.
- Keep initialization, configuration, install/update/removal, service control, console commands, metrics and log access in the binary CLI. Python and documented system tools may be runtime dependencies. Temporary extracted assets must be private, use isolated Python imports, and be cleaned up after use.
- Provide readable human output and terminal colors with `NO_COLOR` support. `--json` must emit valid machine output (NDJSON for logs), including errors, without ANSI escapes or mixed progress text. Secret values must remain redacted in every format.
- Keep source Task wrappers available for the same operations. Treat CLI arguments and `.env` values as literal data, never shell code or Task templates. Do not interpolate Task's `CLI_ARGS`; recover literal arguments through the binary's Task adapter.
- Removal must identify owned installation artifacts, refuse an active/unknown backend, preserve the server/world/mods/backups/private configuration, and offer a preview before explicit removal. Never turn uninstall into a recursive delete of configurable directories.
- `.env` is the single operator-controlled configuration source, including the RCON password. Socket reloads read an explicit resolved path and validate the snapshot; they do not write settings or create a second active config file.
- When adding, renaming, or removing a setting, update Go parsing/defaults/validation, Python schema/defaults/validation, `.env.example`, CLI/Task usage, and relevant documentation together. Keep the example complete and safe, and test schema/default parity.
- During a deployment, bring the installed private `.env` up to date with the same keys while preserving existing operator values, ownership, permissions, and credentials. Never copy the live file into Git, test output, release notes, or packages.
- Make tunable behavior configurable with explicit units and safe defaults. Protocol bounds and defensive implementation limits should remain documented safety limits, not dozens of unnecessary settings.
- Apply supported live settings atomically. Bind/validation failures must preserve the previous configuration and listener. Startup-only changes must explain the required restart or migration.

## Releases and Actions budget

- Ordinary fixes, commits, pushes, deployments, and version-string changes are **not permission to release**. Never create or push a release tag, or publish a release, unless the owner explicitly requests that release.
- Batch useful changes into intentional SemVer releases. Stable tags use `vMAJOR.MINOR.PATCH`, for example `v0.4.0`; no leading zeroes. Do not create a tag for each edit or failed workflow attempt unless the owner explicitly authorized corrective release tags for that attempt.
- Before an approved tag: review the diff, run `task check`, and build/inspect `task release:package VERSION=vX.Y.Z`, including its generated `RELEASE_NOTES.md`. Packaging is local and never creates tags, pushes, or releases.
- Published tags and assets are immutable by policy. Never move/delete/recreate a published tag or overwrite its release assets. Fixes normally belong in a later, separately approved version. Recover an interrupted unpublished draft deliberately; do not silently replace it.
- Keep the workflow tag-only: no branch/PR builds, schedules, automatic version bumps, or automatic tagging. One standard Linux runner, one test pass, two Linux standalone binaries, and a 10-minute job limit. No build matrix, containers, or Actions artifact uploads/caches without an explicit budget decision.
- Write useful commit subjects: release packaging generates the **Changelog** from Git history since the previous reachable stable release tag. Do not maintain `CHANGELOG.md` or manually invent release highlights. Keep release pages free of Downloads and Install or update sections, generation-method boilerplate, and architecture-test disclaimers; installation instructions and generation policy belong in README/this file.
- Keep actions pinned to reviewed commit SHAs. Publish raw standalone executables, checksums, version/commit/toolchain metadata, and generated change notes. Operator scripts/templates are embedded, never a source-download prerequisite. Exclude real `.env`, server/world data, logs, backups, and machine-specific state from every asset.
- Before an approved release, exercise the actual executable from an empty directory with no adjacent scripts, templates, Taskfile or source tree. Verify initialization, configuration, help, output modes and error paths; inspect both target binary headers and checksums.
