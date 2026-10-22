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
