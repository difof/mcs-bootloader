# Minecraft compatibility

mcs-bootloader understands the small handshake/login envelope needed to read the claimed username and send a pre-login reconnect message. After admission, it relays the original handshake, login frame and all subsequent bytes unchanged. Encryption, compression, configuration, gameplay and mod/plugin negotiation remain between the real client and Minecraft.

## Supported versions

The supported envelope range is **stable Minecraft Java 1.7.10 through 26.1.2**. It uses a finite protocol allowlist; snapshots, pre-1.7 protocols and unverified future versions are rejected. Bedrock and protocol translation are outside this gateway's scope. Players still need a version and modpack compatible with the backend.

| Java release | Login fields after the username |
| --- | --- |
| 1.7.10–1.18.2 | None |
| 1.19 | Optional signature record |
| 1.19.1–1.19.2 | Optional signature record and optional UUID |
| 1.19.3–1.20.1 | Optional UUID |
| 1.20.2–26.1.2 | Required UUID |

Transfer intent is accepted from **1.20.5 / protocol 766** onward. The gateway validates these envelopes without authenticating signatures/UUIDs or rewriting them. Minecraft handles final authentication, whitelist enforcement, transfers and version/mod compatibility.

Set `MC_PROTOCOL` to the backend's wire protocol and `MC_VERSION` to its advertised release name. Common examples:

| Java release | `MC_PROTOCOL` |
| --- | ---: |
| 1.7.10 | 5 |
| 1.12.2 | 340 |
| 1.16.5 | 754 |
| 1.18.2 | 758 |
| 1.19 / 1.19.2 / 1.19.3 | 759 / 760 / 761 |
| 1.20.1 / 1.20.2 / 1.20.4 | 763 / 764 / 765 |
| 1.20.5–1.20.6 | 766 |
| 1.21–1.21.1 | 767 |
| 1.21.4 / 1.21.5 / 1.21.8 | 769 / 770 / 772 |
| 1.21.11 | 774 |
| 26.1–26.1.2 | 775 |

The full allowlist lives in [`internal/mcproto/version.go`](../internal/mcproto/version.go). A supported but mismatched client can still trigger a wake; once ready, Minecraft receives its original handshake and decides whether it can join. Changing `MC_PROTOCOL` needs a gateway restart. `MC_VERSION` is an advertised label and can be reloaded live.

## Modpacks, RCON and metrics

Forge hostname extensions, signature/UUID fields and opaque mod data are retained byte-for-byte. This is not a guarantee that every modpack has been tested. Launch the pack once in the foreground, use loopback RCON, and check a real client join after changing Minecraft or its loader.

Lifecycle polling recognizes the standard English player-list responses used by 1.7–1.12, 1.13–1.15 and 1.16+ servers. Unknown/localized/custom responses fail closed: the player count stays unknown and the gateway will not assume it is safe to stop.

Optional telemetry depends on the version and JVM. Older servers may lack modern `list uuids`, `tick query` or player-NBT commands; the corresponding collector reports unavailable instead of inventing values. See [Metrics](metrics.md) for availability/freshness indicators.

## What is verified

Focused fixtures cover all five login layouts, optional fields, malformed/truncated frames, transfer boundaries and unsupported protocol numbers. Existing sleeping/ready mock-server tests exercise 1.12.2, 1.19.2, 1.21.1 and 26.1.2, including exact original-byte forwarding and bidirectional half-close behavior. These are wire-level fixtures, not full Minecraft instances for every release. The existing real deployment uses 1.21.1/NeoForge; other versions need a real client/modpack check before claiming deployment-level validation.

Schemas and native response text were checked against [PrismarineJS/minecraft-data at `1df9432c07c81`](https://github.com/PrismarineJS/minecraft-data/tree/1df9432c07c81b47326e4fb1e06d3c8d723bf4aa): `data/pc/common/protocolVersions.json`, the relevant `protocol.json` and `language.json` files, and `data/dataPaths.json`. Mojang documents transfer intent in the [1.20.5 release notes](https://www.minecraft.net/en-us/article/minecraft-java-edition-1-20-5).
