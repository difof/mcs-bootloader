# Prometheus metrics

Metrics use a separate HTTP listener. The game listener continues to relay the original Minecraft protocol bytes, including modded traffic.

```dotenv
MC_METRICS_ENABLED=true
MC_METRICS_LISTEN=127.0.0.1:9464
MC_METRICS_POLL_INTERVAL=15s
MC_METRICS_PLAYER_DETAILS=true
MC_METRICS_PLAYER_INTERVAL=60s
MC_METRICS_DIRECTORIES='{"world":"/srv/minecraft/world","backups":"/srv/minecraft-backups"}'
MC_METRICS_DIRECTORY_INTERVAL=5m
MC_METRICS_DIRECTORY_TIMEOUT=30s
```

Apply edits with `mcs-bootloader config reload --env /etc/mcs-bootloader/.env`. All metrics settings are live. A port conflict rejects the entire reload and keeps the old listener and applied configuration. Disabling metrics closes its listener and stops polling. `mcs-bootloader metrics` reads the same metrics through the private administration socket, using the daemon's applied configuration.

Example Prometheus scrape configuration:

```yaml
scrape_configs:
  - job_name: mcs-bootloader
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:9464']
```

Use the gateway host's reachable address if Prometheus runs elsewhere. The endpoint has no authentication or TLS; keep the default loopback binding or restrict a deliberately exposed listener to your monitoring system. Player details include names, UUIDs, coordinates and dimensions. Set `MC_METRICS_PLAYER_DETAILS=false` to remove all player/whitelist identity labels and stop player detail commands. Aggregate player counts and whitelist state remain available.

## Collection behavior

- HTTP scrapes read cached data and lightweight gateway/RCON snapshots. They never send RCON commands, start Minecraft, attach to Java or scan the world.
- Background collection reads native whitelist files, Linux process accounting, Go runtime statistics and HotSpot's existing shared performance data. It sends `list uuids` and `tick query` only while the controller reports a ready server. With player details off, it uses `list` instead.
- Online player details use eight fixed, read-only `data get entity <verified-name> <field>` queries. Names come from the strictly validated native UUID list; arbitrary selectors or command text are never accepted. The entire detail batch shares `MC_OPERATION_TIMEOUT`, and the lifecycle state is checked between commands. The existing RCON gate serializes each command with lifecycle and operator commands.
- Detailed fields refresh at most once per `MC_METRICS_PLAYER_INTERVAL`; new players can trigger a fresh batch. The starting player and field rotate between batches so a slow or crowded server cannot permanently starve later entries. Disconnected players disappear after the next successful list poll, and identities are removed immediately when the privacy setting is applied. A JVM PID/start change invalidates detail caches.
- Collection cycles do not overlap. The configured poll interval is the wait after a collection completes. No per-scrape or successful-poll log messages are emitted; failures appear in bounded counters and availability gauges.
- `getconf CLK_TCK` provides the Linux process clock scale once when metrics start. HotSpot collection uses `/proc/<pid>/root/tmp/hsperfdata_*/<pid>`, so a systemd private `/tmp` does not require namespace changes. The gateway needs permission to inspect its configured Java process; using the same OS account provides that on a normal Linux installation.

Native RCON text parsers target the standard English Java server responses, including the tested Minecraft1.21.1 forms. A mod or different version may return unsupported output. That collector becomes unavailable; gameplay and sleep/wake control continue independently. No JVM arguments, JMX agent, Minecraft mods or world files are changed for monitoring.

## Availability and freshness

Every collector has these metrics with a fixed `collector` label:

| Metric | Meaning |
| --- | --- |
| `mcs_metrics_collector_expected` | Whether this source should currently have data. Sleeping Minecraft is not expected to answer RCON. |
| `mcs_metrics_collector_success` | Latest attempt succeeded and its backend state still applies. |
| `mcs_metrics_collector_errors_total` | Failed collection attempts, excluding sources skipped while sleeping or disabled. |
| `mcs_metrics_collector_last_attempt_timestamp_seconds` | Last collection attempt, as Unix time. |
| `mcs_metrics_collector_last_success_timestamp_seconds` | Last successful attempt, omitted until one succeeds. |
| `mcs_metrics_collector_duration_seconds` | Duration of the last attempt. |
| `mcs_metrics_collector_age_seconds` | Age of the last successful attempt. |

Collector names are `self`, `whitelist`, `minecraft_players`, `minecraft_player_details`, `minecraft_ticks`, `minecraft_status`, `jvm_process` and `jvm_perfdata`.

Failed or unavailable server readings are omitted rather than represented as zero. Directory scans retain their last complete successful totals and explicitly report scan failure and sample age, as described below. Player detail fields have individual timestamps because a bounded batch can finish only some fields. Those successful fields remain visible until the next detail batch; failed fields from that batch are absent. `mcs_minecraft_player_details_available=0` marks an incomplete batch. JVM and online-server data are suppressed when the controller reports a stopped/unknown backend, and cached JVM samples are suppressed if systemd's PID changes.

`mcs_metrics_collection_timestamp_seconds` and `mcs_metrics_cache_age_seconds` describe the published background snapshot. `process_metrics_available` distinguishes Linux self-process accounting from the Go runtime statistics that can still be collected without it. `mcs_whitelist_known` reports whether the native whitelist read succeeded; the enabled flag is absent when unknown.

## Metric reference

### Gateway and lifecycle

| Metrics | Meaning |
| --- | --- |
| `mcs_build_info{version}` | Gateway build information. |
| `mcs_configuration_generation` | Applied `.env` configuration generation. |
| `mcs_controller_state{state}` | One-hot state: `unknown`, `asleep`, `starting`, `ready`, `stopping`, `failed`. |
| `mcs_controller_ready`, `mcs_backend_known`, `mcs_backend_running` | Readiness and supervised process state. Running is absent when unknown. |
| `mcs_controller_players_known`, `mcs_controller_players` | The player count used by lifecycle safety checks; count is absent when unknown. |
| `mcs_controller_error` | Whether the current lifecycle snapshot has an error. Error text is not exported. |
| `mcs_gateway_active_connections` | Active/pending backend join reservations. |
| `mcs_controller_last_check_timestamp_seconds`, `mcs_controller_last_success_timestamp_seconds`, `mcs_controller_check_age_seconds` | Lifecycle health-check freshness. |
| `mcs_backend_started_timestamp_seconds`, `mcs_backend_stopping_timestamp_seconds`, `mcs_next_wake_timestamp_seconds` | Recorded lifecycle times, when present. |
| `mcs_minecraft_start_time_seconds`, `mcs_minecraft_uptime_seconds` | Verified Java process start time and elapsed uptime, including loading and shutdown. Gateway restarts do not reset these values. |
| `mcs_controller_ready_since_timestamp_seconds`, `mcs_controller_ready_duration_seconds` | Current uninterrupted ready period observed by this gateway. A gateway restart begins a new observation; this is separate from Java uptime. |
| `mcs_idle_timeout_seconds`, `mcs_idle_since_timestamp_seconds`, `mcs_idle_deadline_timestamp_seconds`, `mcs_idle_remaining_seconds` | Current idle policy and applicable deadline. |
| `mcs_gateway_bytes_total{direction}` | Relayed game bytes, `to_backend`/`to_client`, including successfully replayed login frames. Local status/denial replies are excluded. |

Gateway counters use the prefix `mcs_gateway_` and suffix `_total`:

`accepted_connections`, `capacity_rejections`, `invalid_requests`, `status_requests`, `login_requests`, `wake_requests`, `wake_limited`, `routed_connections`, `backend_errors`, `whitelist_denied`, `whitelist_errors`, `draining_rejections`.

Metrics configuration is exported as `mcs_metrics_enabled`, `mcs_metrics_poll_interval_seconds`, `mcs_metrics_player_interval_seconds` and `mcs_metrics_player_details_enabled`. HTTP instrumentation is `mcs_metrics_http_requests_total`, `mcs_metrics_http_errors_total` and `mcs_metrics_http_duration_seconds_total`.

### Startup estimates

Successful complete boots teach the gateway how long loading normally takes. The private boot-history cache survives gateway restarts. A failed boot or adopting an already-running server does not count as a successful observed boot. Readiness polling adds up to one poll interval to an observed duration under normal operation.

| Metric | Meaning |
| --- | --- |
| `mcs_startup_estimate_known` | Whether successful boot history provides an estimate. |
| `mcs_startup_successes_total` | Complete successful boots observed and retained in the cache; resets if the cache is removed or invalid. |
| `mcs_startup_duration_seconds` | Last complete successful boot duration. Absent before the first observation. |
| `mcs_startup_estimate_seconds` | Smoothed estimate: first observed duration, then 75% previous estimate + 25% newest duration. |
| `mcs_startup_started_timestamp_seconds`, `mcs_startup_elapsed_seconds` | Current gateway-initiated attempt start time and elapsed loading time; present only while starting. |
| `mcs_startup_remaining_seconds` | Learned estimate minus the current attempt's elapsed time, clamped at zero. Absent if no estimate or current attempt is known. |
| `mcs_startup_estimate_overdue` | One when loading has reached or exceeded the estimate. Zero remaining does **not** mean ready. |

Uptime and countdown gauges advance between background polls using cached timestamps; scrapes do no extra process or disk reads. Java uptime is absent while the backend is stopped/unknown, its PID changed, or the latest process sample is unavailable. The server can take longer than its estimate after a mod/configuration change. Use `mcs_controller_ready` to determine readiness. `mcs_backend_started_timestamp_seconds` is the lifecycle controller's recorded start/adoption time, so use `mcs_minecraft_start_time_seconds` for actual Java uptime.

### Directory sizes

`MC_METRICS_DIRECTORIES` is a JSON object of stable names to absolute directory paths. It defaults to `{}`. The service account needs read/traverse permission for every selected directory. Use names such as `world`, `backups` and `mods`; filesystem paths are not exposed as metric labels. Up to 32 roots are supported. Names start with a letter and contain up to 64 ASCII letters, digits, dots, underscores or hyphens.

A separate worker scans these paths without reading file contents and while Minecraft is asleep or running. `MC_METRICS_DIRECTORY_INTERVAL` defaults to `5m` after a scan cycle completes; `MC_METRICS_DIRECTORY_TIMEOUT` defaults to `30s` per root. Scans never run in parallel with another directory scan and cannot delay ordinary player/JVM collection, control commands, or scrapes. Reload cancels the current cycle, removes deleted or changed-path samples immediately, and scans the new configuration. Disabling metrics stops scheduling scans.

Traversal refuses symlinks in the root path and skips discovered symlinks, sockets, devices and FIFOs. Configure the real directory path rather than a symlink. Hardlinked regular files are counted once within each root; overlapping configured roots are independent totals and should not be summed. Subdirectories on a different device are skipped, so configure such mounts as separate roots. A defensive bound of one million entries and 256 nested levels prevents unbounded scans. A limit, permission error, removed file, or timeout fails the complete attempt; partial totals are never published.

The timeout is cooperative between filesystem operations. A hung kernel filesystem operation can take longer, particularly on an unavailable network mount. There is still only one directory worker, and shutdown does not wait for that worker. Prefer local directories for predictable collection latency. These are sampled totals, not a filesystem snapshot: files may change while a scan runs.

All per-root metrics use the `directory` label:

| Metric | Meaning |
| --- | --- |
| `mcs_directory_size_bytes` | Logical size of unique regular files from the last complete successful scan. |
| `mcs_directory_allocated_bytes` | Allocated file blocks × 512; sparse files can be smaller than their logical size. Directory metadata is excluded, and filesystem compression/reflinks can affect interpretation. |
| `mcs_directory_files`, `mcs_directory_directories` | Unique regular-file and directory counts; directory count includes the root. |
| `mcs_directory_symlinks_skipped`, `mcs_directory_special_files_skipped`, `mcs_directory_mounts_skipped` | Exclusions from the last successful scan. |
| `mcs_directory_scan_success` | Whether the last completed attempt succeeded. Zero before the first successful attempt or after an error. |
| `mcs_directory_scan_in_progress` | Whether this root is currently being scanned. |
| `mcs_directory_scan_errors_total` | Failed complete attempts; reload/shutdown cancellations are excluded. |
| `mcs_directory_scan_duration_seconds`, `mcs_directory_scan_last_attempt_timestamp_seconds` | Latest completed attempt duration and latest attempt start time. |
| `mcs_directory_scan_last_success_timestamp_seconds`, `mcs_directory_scan_age_seconds` | Completion time and age of the retained successful totals. |

Sizes/counts are absent until the first complete scan succeeds. On later failures, the last good values remain, accompanied by `scan_success=0` and increasing age; do not mistake them for a current measurement or an empty folder. Removing a root or changing its path clears its cache and counters. The listener also exposes `mcs_metrics_directories_configured`, `mcs_metrics_directory_interval_seconds` and `mcs_metrics_directory_timeout_seconds`.

### RCON

The operation label is restricted to `list`, `list_uuids`, `tick_query`, `player_data`, `stop`, `kick` and `operator`. Arbitrary operator commands are counted under `operator`; their text, arguments, responses and credentials never become metric labels.

| Metrics | Meaning |
| --- | --- |
| `mcs_rcon_connected`, `mcs_rcon_in_flight`, `mcs_rcon_queued` | TCP connection and command/gate activity. |
| `mcs_rcon_requests_total{operation}` | Completed requests, including validation/queueing failures. |
| `mcs_rcon_successes_total`, `mcs_rcon_errors_total`, `mcs_rcon_timeouts_total`, `mcs_rcon_cancellations_total` | Request outcomes by operation. |
| `mcs_rcon_response_bytes_total{operation}` | Successful response body sizes, not contents. |
| `mcs_rcon_request_duration_seconds{operation}` | Histogram including queue wait, authentication and the complete response; exposed as `_bucket`, `_sum`, `_count`. |
| `mcs_rcon_connection_attempts_total`, `mcs_rcon_connections_total`, `mcs_rcon_connection_errors_total`, `mcs_rcon_reconnects_total` | Connection lifecycle. |
| `mcs_rcon_authentication_attempts_total`, `mcs_rcon_authentication_successes_total`, `mcs_rcon_authentication_errors_total` | Authentication exchanges. |

RCON counters survive `.env` password rotation. Failed/uncertain commands are never automatically replayed for telemetry.

### Minecraft, players and whitelist

| Metrics | Meaning |
| --- | --- |
| `mcs_minecraft_players_online`, `mcs_minecraft_players_capacity` | Native list count and capacity. |
| `mcs_minecraft_player_online{name,uuid}` | Current online identities, when details are enabled. |
| `mcs_minecraft_tick_target_rate_hertz` | Configured target tick rate. **This is not measured TPS.** |
| `mcs_minecraft_tick_duration_seconds`, `mcs_minecraft_tick_target_duration_seconds` | Mean execution time and target tick duration. |
| `mcs_minecraft_tick_duration_quantile_seconds{quantile}`, `mcs_minecraft_tick_samples` | Native P50/P95/P99 timing and sample size, when reported. |
| `mcs_minecraft_tick_frozen`, `mcs_minecraft_tick_sprinting` | Native query state, when recognized. |
| `mcs_minecraft_protocol_version`, `mcs_minecraft_server_info{version}`, `mcs_minecraft_status_player_capacity` | Validated cached native status information. |
| `mcs_minecraft_modded`, `mcs_minecraft_advertised_mods`, `mcs_minecraft_advertised_channels` | Optional fields only when explicitly present in native status. |
| `mcs_whitelist_known`, `mcs_whitelist_enabled`, `mcs_whitelist_entries` | Native whitelist state. Entries are read/countable when enabled. |
| `mcs_whitelist_player{name,uuid}` | Native whitelist membership, when enabled and player details are on. |

Detailed online-player gauges have `name` and `uuid` labels:

| Metric | Native field |
| --- | --- |
| `mcs_minecraft_player_health` | `Health`, health points. |
| `mcs_minecraft_player_food_level` | `foodLevel`. |
| `mcs_minecraft_player_food_saturation` | `foodSaturationLevel`. |
| `mcs_minecraft_player_experience_level` | `XpLevel`. |
| `mcs_minecraft_player_experience_total` | `XpTotal`; a gauge because stored experience can decrease. |
| `mcs_minecraft_player_game_mode` | `playerGameType`: survival0, creative1, adventure2, spectator3. |
| `mcs_minecraft_player_position{axis}` | `Pos`, x/y/z coordinates in blocks. |
| `mcs_minecraft_player_dimension_info{dimension}` | `Dimension`, native resource identifier. |
| `mcs_minecraft_player_field_timestamp_seconds{field}` | Last successful read of that individual field. |
| `mcs_minecraft_player_details_available` | All requested fields succeeded in that player's last batch. |
| `mcs_minecraft_player_details_attempt_timestamp_seconds` | Last attempted detail batch. |

Inventory, full entity NBT, chat, achievements, arbitrary mod internals and player network ping are not collected. RCON has no universal structured endpoint for every mod's data. The gateway preserves opaque game traffic and does not inspect/decrypt player packets to manufacture these values.

### JVM and Linux process

Metrics under `mcs_jvm_process_` describe the configured backend's Java main process:

`pid`, `cpu_seconds_total`, `start_time_seconds`, `resident_memory_bytes`, `virtual_memory_bytes`, `threads`, `open_fds`, `max_fds`, `swap_bytes`, `data_memory_bytes`, `stack_memory_bytes`, `context_switches_total{kind}`.

Optional I/O metrics are `io_read_bytes_total`, `io_written_bytes_total`, `io_read_characters_total`, `io_written_characters_total`, `io_read_calls_total`, `io_write_calls_total`. Missing procfs permissions omit the unavailable fields. CPU seconds are cumulative user+system time; use `rate()` to measure CPU usage. `mcs_minecraft_uptime_seconds` provides elapsed Java uptime from the same verified process start timestamp.

HotSpot shared perfdata provides:

| Metrics | Meaning |
| --- | --- |
| `mcs_jvm_memory_pool_used_bytes`, `mcs_jvm_memory_pool_committed_bytes`, `mcs_jvm_memory_pool_max_bytes`, `mcs_jvm_memory_pool_initial_bytes` | Heap spaces labeled by generation and pool. |
| `mcs_jvm_generation_committed_bytes`, `mcs_jvm_generation_min_bytes`, `mcs_jvm_generation_max_bytes` | Heap generation capacities. G1 generation maxima can each cover the entire heap; **do not sum those maxima**. |
| `mcs_jvm_nonheap_used_bytes`, `mcs_jvm_nonheap_committed_bytes`, `mcs_jvm_nonheap_max_bytes`, `mcs_jvm_nonheap_min_bytes` | Metaspace and compressed class space, when available. |
| `mcs_jvm_gc_collections_total{collector,collector_name}`, `mcs_jvm_gc_duration_seconds_total{collector,collector_name}` | GC invocation counts and time, converted using the JVM's reported clock frequency. |
| `mcs_jvm_threads_live`, `mcs_jvm_threads_daemon`, `mcs_jvm_threads_peak`, `mcs_jvm_threads_started_total` | Java thread counts. |
| `mcs_jvm_classes_loaded_total`, `mcs_jvm_classes_unloaded_total`, `mcs_jvm_classes_currently_loaded` | Class-loading counters and their current difference. |
| `mcs_jvm_safepoints_total`, `mcs_jvm_safepoint_seconds_total`, `mcs_jvm_safepoint_sync_seconds_total`, `mcs_jvm_application_seconds_total` | HotSpot safepoint/application time. |
| `mcs_jvm_jit_compilations_total`, `mcs_jvm_jit_bailouts_total`, `mcs_jvm_jit_compilation_seconds_total` | JIT counters/time. |
| `mcs_jvm_info{version,name,vendor}` | Selected VM implementation information, when present. |
| `mcs_jvm_perfdata_used_bytes`, `mcs_jvm_perfdata_overflow_bytes`, `mcs_jvm_perfdata_counters` | Shared buffer diagnostics. |
| `mcs_jvm_perfdata_value{counter,unit,variability}` | Every supported numeric scalar in the shared buffer, in its original unit. |

The last family is deliberately a **gauge of raw values**. `unit` is `none`, `bytes`, `ticks`, `events` or `hertz`; `variability` is `constant`, `monotonic` or `variable`. Do not assume raw `ticks` are seconds or every value increases. Internal counter names vary across JDKs/collectors; use the normalized metric families where possible. Raw strings containing JVM arguments, application commands, filesystem paths or class/method names are excluded. Only selected VM version/vendor and GC/pool names are retained.

If the backend's main process is a wrapper rather than Java, procfs access is denied, shared perfdata is disabled, the format is unsupported, or PID/start identity cannot be verified, the affected JVM collectors report unavailable. The exporter does not alter launcher/JVM flags or use an attach tool to overcome this. Heap/GC figures are sampled from the JVM's own existing counters and have their native update cadence.

### Gateway's own Go process

`process_` exports the same Linux accounting suffixes listed for `mcs_jvm_process_`, plus `process_uptime_seconds` from the cached process start timestamp. `go_info{version}`, `go_goroutines`, `go_threads` and `go_gomaxprocs` describe the Go runtime.

Memory metrics use `go_memstats_`: `alloc_bytes`, `alloc_bytes_total`, `sys_bytes`, `mallocs_total`, `frees_total`, `heap_alloc_bytes`, `heap_sys_bytes`, `heap_idle_bytes`, `heap_inuse_bytes`, `heap_released_bytes`, `heap_objects`, `stack_inuse_bytes`, `stack_sys_bytes`, `mspan_inuse_bytes`, `mcache_inuse_bytes`, `gc_sys_bytes`, `next_gc_bytes`.

GC metrics are `go_gc_cycles_total`, `go_gc_forced_cycles_total`, `go_gc_pause_seconds_total`, `go_gc_cpu_fraction` and `go_gc_last_timestamp_seconds`.

## Useful queries

```promql
# JVM CPU cores consumed, averaged over five minutes
rate(mcs_jvm_process_cpu_seconds_total[5m])

# Used JVM heap, summing actual heap spaces
sum by (instance) (mcs_jvm_memory_pool_used_bytes)

# Minecraft uptime in hours, unchanged when the gateway restarts
mcs_minecraft_uptime_seconds / 3600

# Loading attempts that have exceeded their learned estimate
mcs_startup_estimate_overdue == 1

# Failed collectors that should currently be available
(mcs_metrics_collector_expected == 1)
  and on (job, instance, collector)
(mcs_metrics_collector_success == 0)

# Age of each player's health reading
time() - mcs_minecraft_player_field_timestamp_seconds{field="Health"}

# Last successful logical size per configured root, in GiB
mcs_directory_size_bytes / 1024^3

# Directory roots whose latest completed scan failed
mcs_directory_scan_success == 0

# Gateway p95 RCON request duration by operation
histogram_quantile(0.95,
  sum by (instance, operation, le)
    (rate(mcs_rcon_request_duration_seconds_bucket[5m])))
```

The HTTP body follows the [Prometheus0.0.4 text exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/). The read-only HotSpot parser follows OpenJDK's [perfdata buffer layout](https://github.com/openjdk/jdk21u/blob/master/src/hotspot/share/runtime/perfMemory.hpp) and [counter units/variability](https://github.com/openjdk/jdk21u/blob/master/src/hotspot/share/runtime/perfData.hpp).
