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

## Releases and Actions budget

- Ordinary fixes, commits, pushes, deployments, and version-string changes are **not permission to release**. Never create or push a release tag, or publish a release, unless the owner explicitly requests that release.
- Batch useful changes into intentional SemVer releases. Stable tags use `vMAJOR.MINOR.PATCH`, for example `v0.3.0`; no leading zeroes. Do not invent a new tag for each edit or failed workflow attempt.
- Before an approved tag: review the diff, update `CHANGELOG.md` (including upgrade notes), run `task check`, and build/inspect `task release:package VERSION=vX.Y.Z`. Packaging is local and never creates tags, pushes, or releases.
- Published tags and assets are immutable by policy. Never move/delete/recreate a published tag or overwrite its release assets. Fixes normally belong in a later, separately approved version. Recover an interrupted unpublished draft deliberately; do not silently replace it.
- Keep the workflow tag-only: no branch/PR builds, schedules, automatic version bumps, or automatic tagging. One standard Linux runner, one test pass, two cross-compiled Linux packages, and a 10-minute job limit. No build matrix, containers, or Actions artifact uploads/caches without an explicit budget decision.
- Keep actions pinned to reviewed commit SHAs. Include installation files, checksums, version/commit/toolchain metadata, release highlights, and generated change notes. Exclude real `.env`, server/world data, logs, backups, and machine-specific state from every package.
