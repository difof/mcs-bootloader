package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testEnvPassword = "0123456789abcdef-env-test-only"

func envWith(body string) []byte { return []byte("MC_RCON_PASSWORD=" + testEnvPassword + "\n" + body) }

func TestLoadEnvSharedSchemaAndLiterals(t *testing.T) {
	data := envWith(`
# Metadata and runtime settings share one file.
export MC_LISTEN = ":25565" # public gateway
MC_BACKEND_ADDRESS='127.0.0.1:25566'
MC_RCON_ADDRESS=127.0.0.1:25575
MC_BACKEND_SERVICE=mc-gateway-backend.service
MC_STATE_DIRECTORY=/var/lib/mc-gateway
MC_ADMIN_SOCKET=/run/mc-gateway/control.sock
MC_SERVER_DIRECTORY=/srv/minecraft
MC_JAVA_BINARY=/usr/bin/java
MC_JVM_ARGFILE=@user_jvm_args.txt
MC_SERVER_ARGFILE=@libraries/loader/unix_args.txt
MC_PROTOCOL=000767
MC_VERSION=1.21.1
MC_MAX_PLAYERS="8"
MC_MAX_CONNECTIONS=64
MC_IDLE_TIMEOUT=2h
MC_POLL_INTERVAL=30s
MC_OPERATION_TIMEOUT=6s
MC_STARTUP_TIMEOUT=15m
MC_SHUTDOWN_WARN_AFTER=3m
MC_WAKE_COOLDOWN=30s
MC_HANDSHAKE_TIMEOUT=5s
MC_DIAL_TIMEOUT=3s
MC_BOOT_MESSAGE="Booting\nPlease wait \ud83d\ude80"
MC_SLEEPING_MOTD='sleep $HOME $(do-not-run) # literal'
MC_STARTING_MOTD=Booting#literal
MC_FAILED_MOTD=Try later # ignored comment
MC_LOG_LEVEL=debug
MC_BINARY=/opt/mc-gateway/bin/mc-gateway
MC_CONFIG=/etc/mc-gateway/.env
MC_SERVICE_USER=compute
MC_GATEWAY_SERVICE=mc-gateway.service
MC_LOG_NAMESPACE=mc-gateway
MC_LOG_LINES=100
MC_COMMAND_TIMEOUT=30s
MC_BACKUP_DIRECTORY=/srv/minecraft-backups/pre-gateway
`)
	cfg, err := LoadEnv(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RCONPasswordFile != "" {
		t.Fatal("dotenv retained a separate password file")
	}
	password, err := cfg.Password()
	if err != nil || password != testEnvPassword {
		t.Fatal("inline password changed")
	}
	if cfg.Protocol != 767 || cfg.MaxPlayers != 8 || cfg.IdleTimeout != Duration(2*time.Hour) || cfg.PollInterval != Duration(30*time.Second) {
		t.Fatal("typed settings did not load")
	}
	if cfg.ServerArgFile != "@libraries/loader/unix_args.txt" || cfg.LogLevel != "debug" {
		t.Fatal("startup/log settings did not load")
	}
	if cfg.BootMessage != "Booting\nPlease wait 🚀" || cfg.SleepingMOTD != "sleep $HOME $(do-not-run) # literal" || cfg.StartingMOTD != "Booting#literal" || cfg.FailedMOTD != "Try later" {
		t.Fatal("literal or message parsing changed")
	}
}

func TestLoadEnvRejectsInvalidInputWithoutValues(t *testing.T) {
	secret := "DO_NOT_ECHO_CONFIG_VALUE_12345"
	cases := map[string][]byte{
		"missing secret":         []byte("MC_IDLE_TIMEOUT=1h\n"),
		"legacy secret path":     envWith("MC_RCON_PASSWORD_FILE=" + secret),
		"unknown":                envWith("MC_UNKNOWN=" + secret),
		"duplicate":              envWith("MC_RCON_PASSWORD=" + secret),
		"invalid assignment":     envWith("not an assignment " + secret),
		"CRLF":                   envWith("MC_VERSION=" + secret + "\r\n"),
		"NUL":                    envWith("MC_VERSION=" + secret + "\x00"),
		"invalid UTF8":           append(envWith("MC_VERSION="+secret), 0xff),
		"unterminated single":    envWith("MC_VERSION='" + secret),
		"unterminated double":    envWith("MC_VERSION=\"" + secret),
		"no quote comment space": envWith("MC_VERSION=\"" + secret + "\"#comment"),
		"trailing quote text":    envWith("MC_VERSION=\"" + secret + "\" extra"),
		"empty":                  envWith("MC_VERSION=# " + secret),
		"integer":                envWith("MC_MAX_PLAYERS=" + secret),
		"integer negative":       envWith("MC_MAX_PLAYERS=-2"),
		"integer plus":           envWith("MC_MAX_PLAYERS=+2"),
		"integer exponent":       envWith("MC_MAX_PLAYERS=1e2"),
		"integer overflow":       envWith("MC_LOG_LINES=9223372036854775808"),
		"duration":               envWith("MC_IDLE_TIMEOUT=" + secret),
		"address":                envWith("MC_BACKEND_ADDRESS=" + secret),
		"newline nonmessage":     envWith(`MC_VERSION="` + secret + `\n"`),
		"control":                envWith(`MC_VERSION="` + secret + `\b"`),
		"high surrogate":         envWith(`MC_VERSION="` + secret + `\ud800"`),
		"low surrogate":          envWith(`MC_VERSION="` + secret + `\udfff"`),
		"bad surrogate pair":     envWith(`MC_VERSION="` + secret + `\ud800\u0041"`),
		"too large":              envWith("#" + strings.Repeat("x", maxEnvBytes)),
		"short password":         []byte("MC_RCON_PASSWORD=short\n"),
		"long password":          []byte("MC_RCON_PASSWORD=" + strings.Repeat("p", 4097)),
		"newline password":       []byte(`MC_RCON_PASSWORD="` + secret + `\n"`),
		"escaped CR password":    []byte(`MC_RCON_PASSWORD="` + secret + `\r"`),
		"escaped NUL password":   []byte(`MC_RCON_PASSWORD="` + secret + `\u0000"`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadEnv(data)
			if err == nil {
				t.Fatal("invalid environment was accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testEnvPassword) {
				t.Fatal("diagnostic exposed a setting value")
			}
		})
	}
}

func TestLoadEnvPreservesPasswordBytes(t *testing.T) {
	password := "  literal $HOME $(ignored) `ignored` \\ #=:'\t🚀  "
	encoded, err := json.Marshal(password)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadEnv([]byte("MC_RCON_PASSWORD=" + string(encoded) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Password()
	if err != nil || got != password {
		t.Fatal("password was trimmed, expanded, or changed")
	}
	for _, encoded := range []string{cfg.String(), cfg.GoString(), cfg.LogValue().String(), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(encoded, "ignored") {
			t.Fatal("configuration formatting exposed the password")
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ignored") || strings.Contains(string(raw), `"RCONPassword"`) {
		t.Fatal("JSON exposed inline password")
	}
	var roundTrip Config
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.IdleTimeout != cfg.IdleTimeout || roundTrip.OperationTimeout != cfg.OperationTimeout {
		t.Fatal("duration JSON did not round trip")
	}
}

func TestLoadConfigEnvPermissionsContentDetectionAndSymlink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "arbitrary-private-snapshot")
	if err := os.WriteFile(path, envWith("MC_IDLE_TIMEOUT=2h"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0640, 0644, 0660, 0400, 0755} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if (mode == 0600 || mode == 0640) != (err == nil) {
			t.Fatalf("unexpected permission result for %04o: %v", mode, err)
		}
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, ".env")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(directory); err == nil {
		t.Fatal("directory accepted as configuration")
	}
	legacy := filepath.Join(directory, "legacy-without-json-extension")
	if err := os.WriteFile(legacy, []byte(`{"idle_timeout":"2h"}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != Duration(2*time.Hour) || cfg.RCONPasswordFile == "" || cfg.RCONPassword != "" {
		t.Fatal("legacy JSON migration is not preserved")
	}
}

func TestInlinePasswordValidationAndPortAliases(t *testing.T) {
	for _, password := range []string{"short", strings.Repeat("p", 4097), testEnvPassword + "\n", testEnvPassword + "\r", testEnvPassword + "\x00"} {
		cfg := DefaultConfig()
		cfg.RCONPassword = password
		if _, err := cfg.Password(); err == nil {
			t.Fatal("invalid inline password accepted")
		}
	}
	cfg := DefaultConfig()
	cfg.RCONAddress = "127.0.0.1:025566"
	if cfg.Validate() == nil {
		t.Fatal("aliased duplicate private port accepted")
	}
}
