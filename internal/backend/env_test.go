package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
MC_BACKEND_SERVICE=mcs-bootloader-backend.service
MC_STATE_DIRECTORY=/var/lib/mcs-bootloader
MC_ADMIN_SOCKET=/run/mcs-bootloader/control.sock
MC_SERVER_DIRECTORY=/srv/minecraft
MC_LAUNCH_EXECUTABLE=/usr/bin/java
MC_LAUNCH_ARGS='["@user_jvm_args.txt","@libraries/loader/unix_args.txt","nogui"]' 
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
MC_BINARY=/opt/mcs-bootloader/bin/mcs-bootloader
MC_CONFIG=/etc/mcs-bootloader/.env
MC_SERVICE_USER=gamesvc
MC_GATEWAY_SERVICE=mcs-bootloader.service
MC_LOG_NAMESPACE=mcs-bootloader
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
	if cfg.LaunchExecutable != "/usr/bin/java" || !reflect.DeepEqual(cfg.LaunchArgs, []string{"@user_jvm_args.txt", "@libraries/loader/unix_args.txt", "nogui"}) || cfg.LogLevel != "debug" {
		t.Fatal("startup/log settings did not load")
	}
	if cfg.BootMessage != "Booting\nPlease wait 🚀" || cfg.SleepingMOTD != "sleep $HOME $(do-not-run) # literal" || cfg.StartingMOTD != "Booting#literal" || cfg.FailedMOTD != "Try later" {
		t.Fatal("literal or message parsing changed")
	}
}

func TestLiveDisplayMetricsAndRestartEnvironment(t *testing.T) {
	cfg, err := LoadEnv(envWith(`MC_OVERRIDE_MOTD=TrUe
MC_MOTD="Friends\nHeavy modpack"
MC_RESTART_MESSAGE="Gateway update in progress; reconnect shortly"
MC_RESTART_GRACE=8s
MC_WHITELIST_MESSAGE="Friends only\nAsk the owner"
MC_STOPPING_MOTD="Saving the world"
MC_UNKNOWN_MOTD="Checking Minecraft"
MC_METRICS_ENABLED=TRUE
MC_METRICS_LISTEN=127.0.0.1:9465
MC_METRICS_POLL_INTERVAL=30s
MC_METRICS_PLAYER_INTERVAL=2m
MC_METRICS_PLAYER_DETAILS=FALSE
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OverrideMOTD || cfg.MOTD != "Friends\nHeavy modpack" || cfg.RestartGrace != Duration(8*time.Second) || cfg.WhitelistMessage != "Friends only\nAsk the owner" || cfg.StoppingMOTD != "Saving the world" || cfg.UnknownMOTD != "Checking Minecraft" {
		t.Fatal("live display configuration did not parse exactly")
	}
	if !cfg.MetricsEnabled || cfg.MetricsListen != "127.0.0.1:9465" || cfg.MetricsPollInterval != Duration(30*time.Second) || cfg.MetricsPlayerInterval != Duration(2*time.Minute) || cfg.MetricsPlayerDetails {
		t.Fatal("typed metrics configuration did not parse exactly")
	}
	for _, body := range []string{
		"MC_OVERRIDE_MOTD=1", "MC_OVERRIDE_MOTD=yes", "MC_METRICS_ENABLED=on", "MC_METRICS_PLAYER_DETAILS=0",
		`MC_RESTART_MESSAGE="first\nsecond"`, `MC_RESTART_MESSAGE="first\rsecond"`, `MC_RESTART_MESSAGE="first\u0000second"`,
		"MC_RESTART_MESSAGE=" + strings.Repeat("x", 4097), "MC_MOTD=" + strings.Repeat("界", 1366),
		"MC_RESTART_GRACE=0s", "MC_METRICS_POLL_INTERVAL=0s", "MC_METRICS_PLAYER_INTERVAL=0s", "MC_METRICS_LISTEN=localhost:9464", "MC_METRICS_LISTEN=127.0.0.1:0",
		"MC_METRICS_ENABLED=true\nMC_METRICS_LISTEN=127.0.0.1:25575",
		"MC_METRICS_ENABLED=true\nMC_METRICS_LISTEN=127.0.0.1:25566",
		"MC_METRICS_ENABLED=true\nMC_METRICS_LISTEN=0.0.0.0:25565",
	} {
		if _, err := LoadEnv(envWith(body)); err == nil {
			t.Fatalf("invalid display/metrics configuration accepted: %s", strings.Split(body, "=")[0])
		}
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

func TestLaunchArgumentsAreLiteralTypedAndBounded(t *testing.T) {
	want := []string{"", "space argument", "@unread-file", "${HOME}", "$(touch never)", "literal ' quote", "🚀"}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	// JSON double quoting also permits an argument containing an apostrophe;
	// no shell parser participates in either decoding layer.
	quoted, err := json.Marshal(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadEnv(envWith("MC_LAUNCH_EXECUTABLE=./launch.sh\nMC_LAUNCH_ARGS=" + string(quoted)))
	if err != nil || cfg.LaunchExecutable != "./launch.sh" || !reflect.DeepEqual(cfg.LaunchArgs, want) {
		t.Fatalf("literal argument parsing failed: %v", err)
	}
	tooMany, _ := json.Marshal(make([]string, maxLaunchArgs+1))
	tooLarge, _ := json.Marshal([]string{strings.Repeat("x", maxLaunchBytes+1)})
	for _, value := range []string{
		`null`, `{}`, `"-jar server.jar"`, `[null]`, `[1]`, `[true]`, `[["nested"]]`,
		`["a"] false`, `["\u0000"]`, `["line\nfeed"]`, `["\r"]`,
		`["\ud800"]`, `["\udfff"]`, `["\ud800\u0041"]`, string(tooMany), string(tooLarge),
	} {
		if _, err := LoadEnv(envWith("MC_LAUNCH_ARGS='" + value + "'")); err == nil {
			t.Errorf("invalid launch arguments were accepted: %.80s", value)
		}
	}
	cfg, err = LoadEnv(envWith("MC_LAUNCH_ARGS=[]"))
	if err != nil || cfg.LaunchArgs == nil || len(cfg.LaunchArgs) != 0 {
		t.Fatalf("empty argument array was not preserved: %v", err)
	}
	if got := cfg.Clone(); got.LaunchArgs == nil {
		t.Fatal("cloning an empty array turned it into null")
	}
}
