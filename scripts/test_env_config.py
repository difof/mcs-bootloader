from pathlib import Path
import json
import os
import re
import tempfile
import unittest
from unittest.mock import patch

import env_config


ROOT = Path(__file__).resolve().parent.parent


class EnvConfigTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / ".env"

    def read(self, text):
        self.path.write_bytes(text if isinstance(text, bytes) else text.encode("utf-8"))
        return env_config.read_env(self.path)

    def test_assignments_comments_export_quotes_and_integers(self):
        result = self.read(
            "# Only explicitly supplied settings are returned\n"
            " export MC_IDLE_TIMEOUT = 1h # a full hour\n"
            "MC_PROTOCOL=767\n"
            "MC_MAX_PLAYERS='005'\n"
            "MC_LOG_LINES=\"100\"\n"
            "MC_VERSION=custom#literal\n"
            "MC_BOOT_MESSAGE='Ready # for friends' # comment\n"
            'MC_STARTING_MOTD="Booting \\\"soon\\\""\n'
        )
        self.assertEqual(result, {
            "MC_IDLE_TIMEOUT": "1h", "MC_PROTOCOL": 767,
            "MC_MAX_PLAYERS": 5, "MC_LOG_LINES": 100,
            "MC_VERSION": "custom#literal",
            "MC_BOOT_MESSAGE": "Ready # for friends",
            "MC_STARTING_MOTD": 'Booting "soon"',
        })

    def test_shell_syntax_is_literal_and_never_executed(self):
        marker = Path(self.temporary.name) / "must-not-exist"
        payload = f"$(touch {marker}) `touch {marker}` $ENV_CONFIG_SENTINEL; & | > *"
        with patch.dict(os.environ, {"ENV_CONFIG_SENTINEL": "expanded"}), \
                patch("os.system", side_effect=AssertionError("shell execution")), \
                patch("os.popen", side_effect=AssertionError("shell execution")), \
                patch("subprocess.run", side_effect=AssertionError("subprocess execution")):
            result = self.read(f"MC_BOOT_MESSAGE='{payload}'\nMC_VERSION=${{ENV_CONFIG_SENTINEL}}\n")
        self.assertEqual(result["MC_BOOT_MESSAGE"], payload)
        self.assertEqual(result["MC_VERSION"], "${ENV_CONFIG_SENTINEL}")
        self.assertFalse(marker.exists())

    def test_json_escaped_message_newline_and_unicode_round_trip(self):
        result = self.read('MC_BOOT_MESSAGE="Booting\\nPlease wait — arkadaşlar"\n')
        self.assertEqual(result["MC_BOOT_MESSAGE"], "Booting\nPlease wait — arkadaşlar")
        text = env_config.serialize_env(result)
        self.assertEqual(text.count("\n"), 1)
        self.assertEqual(self.read(text), result)

    def test_serialization_preserves_quotes_backslashes_and_shell_symbols(self):
        values = {
            "MC_BOOT_MESSAGE": 'It\'s "quoted" # $HOME `true` $(true) \\ path\nNext line',
            "MC_VERSION": " version with spaces ",
            "MC_PROTOCOL": 767,
            "MC_RCON_PASSWORD": ' private # $HOME `true` $(true) " \\ password ',
        }
        self.assertEqual(self.read(env_config.serialize_env(values)), values)

    def test_booleans_are_explicit_typed_and_round_trip(self):
        values = self.read("MC_OVERRIDE_MOTD=TRUE\nMC_METRICS_ENABLED='false'\n"
                           'MC_METRICS_PLAYER_DETAILS="True"\n')
        self.assertEqual(values, {"MC_OVERRIDE_MOTD": True, "MC_METRICS_ENABLED": False,
                                  "MC_METRICS_PLAYER_DETAILS": True})
        self.assertEqual(self.read(env_config.serialize_env(values)), values)
        self.assertEqual(env_config.render_config(values, {})["metrics_enabled"], False)
        for invalid in ("1", "0", "t", "f", "yes", "off", " true ", 1, 0, None, [], {}):
            with self.subTest(invalid=invalid), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values({"MC_METRICS_ENABLED": invalid})

    def test_restart_reason_is_single_line_and_messages_have_utf8_bounds(self):
        for invalid in ("first\nsecond", "first\rsecond", "first\x00second", "é" * 2049):
            with self.subTest(invalid=invalid[:25]), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values({"MC_RESTART_MESSAGE": invalid})
        self.assertEqual(env_config.validate_values({"MC_MOTD": "First\nSecond"}),
                         {"MC_MOTD": "First\nSecond"})
        for key in env_config.MESSAGE_KEYS | {"MC_RESTART_MESSAGE"}:
            with self.subTest(key=key):
                self.assertEqual(env_config.validate_values({key: "é" * 2048})[key], "é" * 2048)
                with self.assertRaisesRegex(env_config.EnvConfigError, "4096"):
                    env_config.validate_values({key: "é" * 2049})

    def test_all_go_configuration_fields_are_mapped(self):
        source = (ROOT / "internal/backend/config.go").read_text()
        struct = re.search(r"type Config struct \{(.*?)\n\}", source, re.DOTALL)
        self.assertIsNotNone(struct)
        fields = set(re.findall(r'json:"([a-z_]+)(?:,[^"]*)?"', struct.group(1)))
        # The legacy password-file field is only for migration compatibility;
        # the inline password deliberately has no Go JSON representation.
        fields.discard("rcon_password_file")
        fields.add("rcon_password")
        self.assertEqual(set(env_config.CONFIG_KEYS.values()), fields)
        self.assertEqual(len(env_config.CONFIG_KEYS), len(fields))

    def test_example_covers_every_setting_and_matches_safe_defaults(self):
        values = env_config.read_env(ROOT / ".env.example")
        self.assertEqual(set(values), env_config.SUPPORTED_KEYS)
        self.assertEqual({key: values[key] for key in env_config.RUNTIME_DEFAULTS},
                         env_config.RUNTIME_DEFAULTS)
        self.assertEqual({key: values[key] for key in env_config.MANAGE_DEFAULTS},
                         env_config.MANAGE_DEFAULTS)

        self.assertEqual(values["MC_CONFIG"], "/etc/mcs-bootloader/.env")
        self.assertNotIn("MC_RCON_PASSWORD_FILE", values)

    def test_config_mapping_preserves_base_values_and_excludes_task_options(self):
        original = {"listen": ":25590", "max_players": 23, "version": "Friends modpack"}
        snapshot = dict(original)
        values = {"MC_IDLE_TIMEOUT": "2h", "MC_BOOT_MESSAGE": "Waking up",
                  "MC_LOG_LINES": "250", "MC_CONFIG": "/etc/example/.env"}
        result = env_config.render_config(values, original)
        self.assertEqual(original, snapshot)
        expected = {**snapshot, "idle_timeout": "2h", "boot_message": "Waking up"}
        self.assertEqual(result, expected)
        self.assertEqual(values["MC_LOG_LINES"], "250")
        self.assertIsNot(result, original)

    def test_seed_from_runtime_fields_is_lossless(self):
        original = {"max_players": 19, "boot_message": "One\nTwo $HOME",
                    "rcon_password": "synthetic-private-password",
                    "launch_executable": "./custom launcher",
                    "launch_args": ["--name", "value with spaces", "", "$(never execute)"]}
        values = env_config.values_from_config(original)
        self.assertEqual(env_config.render_config(self.read(env_config.serialize_env(values)), {}), original)
        self.assertEqual({key: values[key] for key in env_config.MANAGE_DEFAULTS},
                         env_config.MANAGE_DEFAULTS)
        without_defaults = env_config.values_from_config({"version": "custom"},
                                                        include_manage_defaults=False)
        self.assertEqual(without_defaults, {"MC_VERSION": "custom"})

    def test_display_redacts_password_and_includes_effective_defaults(self):
        secret = "DO_NOT_DISPLAY_THIS_SECRET"
        values = {"MC_RCON_PASSWORD": secret, "MC_IDLE_TIMEOUT": "2h"}
        displayed = env_config.redacted_values(values)
        self.assertEqual(displayed["MC_RCON_PASSWORD"], "[REDACTED]")
        self.assertEqual(displayed["MC_IDLE_TIMEOUT"], "2h")
        self.assertEqual(displayed["MC_ADMIN_SOCKET"], "/run/mcs-bootloader/control.sock")
        self.assertEqual(displayed["MC_CONFIG"], "/etc/mcs-bootloader/.env")
        self.assertNotIn(secret, json.dumps(displayed))
        self.assertEqual(values["MC_RCON_PASSWORD"], secret)
        self.assertEqual(env_config.redacted_values({})["MC_LAUNCH_ARGS"], '["-jar","server.jar","nogui"]')

    def test_launch_arguments_are_literal_typed_bounded_json_arrays(self):
        arguments = ["@loader/args.txt", "two words", "", "$(literal)", "{{.UNCHANGED}}"]
        raw = json.dumps(arguments)
        self.assertEqual(env_config.launch_arguments(raw), arguments)
        self.assertEqual(env_config.render_config({"MC_LAUNCH_ARGS": raw}, {}), {"launch_args": arguments})
        self.assertEqual(env_config.launch_arguments("[]"), [])
        for invalid in ("null", "{}", '[null]', '[1]', '[true]', '"-jar server.jar"',
                        '["line\\nfeed"]', '["nul\\u0000"]', '["\\ud800"]',
                        json.dumps([""] * 257), json.dumps(["é" * 16385])):
            with self.subTest(invalid=invalid[:50]), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values({"MC_LAUNCH_ARGS": invalid})

    def test_metrics_directories_are_bounded_literal_named_paths(self):
        paths = {"world": "/srv/minecraft/world", "backups": "/srv/backup $literal"}
        raw = json.dumps(paths)
        self.assertEqual(env_config.metrics_directories(raw), paths)
        self.assertEqual(env_config.render_config({"MC_METRICS_DIRECTORIES": raw}, {}),
                         {"metrics_directories": paths})
        seeded = env_config.values_from_config({"metrics_directories": paths},
                                               include_manage_defaults=False)
        self.assertEqual(env_config.render_config(seeded, {}), {"metrics_directories": paths})
        self.assertEqual(env_config.metrics_directories("{}"), {})
        cases = ["null", "[]", '{"world":null}', '{"world":3}', '{"world":"relative"}',
                 '{"world":"/"}', '{"world":"//"}', '{"world":"/foo/.."}',
                 '{"bad name":"/srv/world"}', '{"world":"/a","world":"/b"}',
                 '{"world":"/a\\nsecret"}', '{"world":"/a\\ud800"}',
                 json.dumps({"A" * 65: "/srv/world"}), json.dumps({"world": "/" + "a" * 4096}),
                 json.dumps({f"root{i}": "/srv/world" for i in range(33)})]
        for invalid in cases:
            with self.subTest(invalid=invalid[:60]), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values({"MC_METRICS_DIRECTORIES": invalid})

    def test_duplicate_unknown_and_syntax_errors_never_echo_values(self):
        secret = "DO_NOT_ECHO_THIS_VALUE"
        cases = (
            (f"MC_VERSION=first\nMC_VERSION={secret}\n", "line 2: MC_VERSION: duplicate"),
            (f"MC_UNKNOWN={secret}\n", "line 1: MC_UNKNOWN: unknown"),
            (f"PATH={secret}\n", "line 1: PATH: unknown"),
            (f"MC_RCON_PASSWORD_FILE={secret}\n", "line 1: MC_RCON_PASSWORD_FILE: unknown"),
            (f"MC_BACKUP_DIRECTORY={secret}\n", "line 1: MC_BACKUP_DIRECTORY: unknown"),
            (f"MC_RCON_PASSWORD=first\nMC_RCON_PASSWORD={secret}\n",
             "line 2: MC_RCON_PASSWORD: duplicate"),
            (f'MC_RCON_PASSWORD="{secret}\\q"\n', "line 1: MC_RCON_PASSWORD: invalid JSON"),
            (f"= {secret}\n", "line 1: expected"),
            (f"{secret} is not an assignment\n", "line 1: expected"),
            (f"MC_VERSION='{secret}'suffix\n", "line 1: MC_VERSION: unexpected"),
            (f'MC_VERSION="{secret}\\q"\n', "line 1: MC_VERSION: invalid JSON"),
        )
        for text, expected in cases:
            with self.subTest(expected=expected):
                with self.assertRaises(env_config.EnvConfigError) as error:
                    self.read(text)
                self.assertIn(expected, str(error.exception))
                self.assertNotIn(secret, str(error.exception))

    def test_empty_multiline_and_forbidden_characters_fail(self):
        cases = (
            "MC_VERSION=\n", "MC_VERSION= # comment\n", "MC_VERSION=''\n",
            'MC_VERSION="   "\n', "MC_BOOT_MESSAGE='first\nsecond'\n",
            'MC_BOOT_MESSAGE="first\nsecond"\n',
            'MC_VERSION="first\\nsecond"\n',
            'MC_RCON_PASSWORD="first\\nsecond"\n',
            'MC_BOOT_MESSAGE="first\\rsecond"\n',
            'MC_BOOT_MESSAGE="first\\u0000second"\n',
            'MC_BOOT_MESSAGE="first\\bsecond"\n',
            'MC_BOOT_MESSAGE="\\ud800"\n',
            b"MC_VERSION=x\r\n", b"MC_VERSION=x\x00\n", b"MC_VERSION=\xff\n",
        )
        for text in cases:
            with self.subTest(text=repr(text)), self.assertRaises(env_config.EnvConfigError):
                self.read(text)

    def test_integer_data_is_strict_and_bounded(self):
        for value in ("", "0", "-1", "+1", "1.0", "1e2", "0x10", " 10 ",
                      "123_456", "9" * 5000, str(1 << 63), True, False, None, 1.5, [], {}):
            with self.subTest(value=repr(value)[:60]), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values({"MC_LOG_LINES": value})
        self.assertEqual(env_config.validate_values({"MC_PROTOCOL": "767"}), {"MC_PROTOCOL": 767})

    def test_protocol_allowlist_matches_go_and_rejects_unknown_versions(self):
        source = (ROOT / "internal/mcproto/version.go").read_text()
        match = re.search(r"var supportedProtocols = \[\.\.\.\]int\{(.*?)\n\}", source, re.DOTALL)
        self.assertIsNotNone(match)
        protocols = {int(value) for value in re.findall(r"\d+", match.group(1))}
        self.assertEqual(protocols, env_config.SUPPORTED_PROTOCOLS)
        for protocol in (5, 340, 759, 760, 763, 767, 775):
            self.assertEqual(self.read(f"MC_PROTOCOL={protocol}\n"), {"MC_PROTOCOL": protocol})
        for protocol in (4, 6, 999, (1 << 30) | 256):
            with self.subTest(protocol=protocol), self.assertRaisesRegex(env_config.EnvConfigError, "unsupported protocol"):
                self.read(f"MC_PROTOCOL={protocol}\n")

    def test_validation_rejects_unknown_keys_and_nonstring_data(self):
        for values in (None, [], {"MC_UNKNOWN": "value"}, {"PATH": "value"},
                       {1: "value"}, {"MC_VERSION": None}, {"MC_VERSION": 767},
                       {"MC_VERSION": True}, {"MC_VERSION": ["value"]}):
            with self.subTest(values=values), self.assertRaises(env_config.EnvConfigError):
                env_config.validate_values(values)

    def test_nonmapping_and_unknown_base_config_are_rejected_for_seeding(self):
        for base in (None, [], {"unsupported": "private value"}):
            with self.subTest(base=base), self.assertRaises(env_config.EnvConfigError):
                env_config.values_from_config(base)
        with self.assertRaises(env_config.EnvConfigError):
            env_config.render_config({}, [])

    def test_file_limit_is_bytes_and_enforced_before_parsing(self):
        self.assertEqual(self.read(b"#" + b"a" * (env_config.MAX_ENV_BYTES - 1)), {})
        with self.assertRaisesRegex(env_config.EnvConfigError, "exceeds 64 KiB"):
            self.read(b"#" + b"a" * env_config.MAX_ENV_BYTES)
        with self.assertRaisesRegex(env_config.EnvConfigError, "exceeds 64 KiB"):
            self.read("#" + "é" * (env_config.MAX_ENV_BYTES // 2))
        with self.assertRaisesRegex(env_config.EnvConfigError, "exceeds 64 KiB"):
            env_config.serialize_env({"MC_VERSION": "x" * env_config.MAX_ENV_BYTES})


if __name__ == "__main__":
    unittest.main()
