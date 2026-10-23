package backend

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func temporaryLaunchConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RCONPassword = testEnvPassword
	cfg.RCONPasswordFile = ""
	cfg.ServerDirectory = t.TempDir()
	cfg.LaunchExecutable = filepath.Join(cfg.ServerDirectory, "java-test-only")
	cfg.LaunchArgs = []string{"@user_jvm_args.txt", "@libraries/loader/unix_args.txt", "nogui"}
	if err := os.MkdirAll(filepath.Join(cfg.ServerDirectory, "libraries/loader"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name, data string
		mode       os.FileMode
	}{
		{"java-test-only", "not executed by tests", 0700},
		{"user_jvm_args.txt", "-Xmx1G\n", 0600},
		{"libraries/loader/unix_args.txt", "example.launch.Main\n", 0600},
		{"server.properties", "# keep exactly\nonline-mode=true\nrcon.password=old-test-password-0123456789\nmotd=Original server\n", 0640},
	} {
		if err := os.WriteFile(filepath.Join(cfg.ServerDirectory, file.name), []byte(file.data), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	a, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	cfg.BackendAddress, cfg.RCONAddress = a.Addr().String(), b.Addr().String()
	return cfg
}

func TestLaunchPreflightKeepsArgumentsOpaqueAndReservesPorts(t *testing.T) {
	cfg := temporaryLaunchConfig(t)
	path := filepath.Join(cfg.ServerDirectory, "server.properties")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	launch, err := prepareLaunch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.releasePorts()
	want := append([]string{cfg.LaunchExecutable}, cfg.LaunchArgs...)
	if !reflect.DeepEqual(launch.args, want) {
		t.Fatal("launch arguments were changed")
	}
	for _, address := range []string{cfg.BackendAddress, cfg.RCONAddress} {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			listener.Close()
			t.Fatal("preflight did not retain private port reservation")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("preflight changed server.properties")
	}
	current, err := os.Getwd()
	if err != nil || current != cwd {
		t.Fatal("preflight changed the working directory")
	}
	launch.releasePorts()
	for _, address := range []string{cfg.BackendAddress, cfg.RCONAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal("preflight did not release private port reservation")
		}
		listener.Close()
	}
}

func TestLaunchPreflightRefusesOccupiedPortsWithoutMutation(t *testing.T) {
	for _, name := range []string{"backend", "RCON"} {
		t.Run(name, func(t *testing.T) {
			cfg := temporaryLaunchConfig(t)
			address := cfg.BackendAddress
			if name == "RCON" {
				address = cfg.RCONAddress
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			path := filepath.Join(cfg.ServerDirectory, "server.properties")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := prepareLaunch(cfg)
			if err == nil {
				launch.releasePorts()
				t.Fatal("occupied private port allowed a launch")
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatal("diagnostic did not identify occupied port role")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("occupied port preflight changed properties")
			}
			if name == "RCON" {
				check, err := net.Listen("tcp", cfg.BackendAddress)
				if err != nil {
					t.Fatal("failed preflight leaked the backend port reservation")
				}
				check.Close()
			}
		})
	}
}

func TestLaunchPreflightRejectsInvalidStartupFiles(t *testing.T) {
	cases := map[string]func(*Config){
		"missing directory":    func(c *Config) { c.ServerDirectory = filepath.Join(c.ServerDirectory, "missing") },
		"missing executable":   func(c *Config) { c.LaunchExecutable = filepath.Join(c.ServerDirectory, "missing") },
		"non-executable file":  func(c *Config) { c.LaunchExecutable = filepath.Join(c.ServerDirectory, "user_jvm_args.txt") },
		"executable directory": func(c *Config) { c.LaunchExecutable = c.ServerDirectory },
		"empty executable":     func(c *Config) { c.LaunchExecutable = "" },
		"NUL executable":       func(c *Config) { c.LaunchExecutable += "\x00" },
		"null arguments":       func(c *Config) { c.LaunchArgs = nil },
		"newline argument":     func(c *Config) { c.LaunchArgs = []string{"line\nextra"} },
		"NUL argument":         func(c *Config) { c.LaunchArgs = []string{"arg\x00extra"} },
	}

	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := temporaryLaunchConfig(t)
			path := filepath.Join(cfg.ServerDirectory, "server.properties")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			change(&cfg)
			launch, err := prepareLaunch(cfg)
			if err == nil {
				launch.releasePorts()
				t.Fatal("invalid startup setting accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("invalid startup setting changed properties")
			}
		})
	}
}

func TestLaunchPreflightResolvesExecutableAndCopiesLiteralArguments(t *testing.T) {
	cfg := temporaryLaunchConfig(t)
	cfg.LaunchExecutable = "./java-test-only"
	cfg.LaunchArgs = []string{"@missing-is-literal", "", "$(never-expanded)", "a b", "--flag=value"}
	launch, err := prepareLaunch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer launch.releasePorts()
	wantBinary := filepath.Join(cfg.ServerDirectory, "java-test-only")
	want := append([]string{wantBinary}, cfg.LaunchArgs...)
	if launch.binary != wantBinary || !reflect.DeepEqual(launch.args, want) {
		t.Fatal("executable was not resolved relative to the server root or arguments were changed")
	}
	cfg.LaunchArgs[0] = "changed-after-preflight"
	if !reflect.DeepEqual(launch.args, want) {
		t.Fatal("preflight retained mutable caller argument storage")
	}
}

func TestJavaPropertiesPasswordEscapesAndNoop(t *testing.T) {
	password := "  spaces\\literal\t#:=! café 🚀 尾  "
	encoded := encodeJavaProperty(password)
	if !strings.HasPrefix(encoded, `\ `) || !strings.Contains(encoded, `\\literal\t`) || !strings.Contains(encoded, `\uD83D\uDE80`) {
		t.Fatal("Java properties escaping is incomplete")
	}
	decoded, err := decodeJavaProperty([]byte(encoded))
	if err != nil || decoded != password {
		t.Fatal("password did not round trip through Java properties encoding")
	}
	raw := []byte("# keep\r\n rcon.password : " + encoded + "\r\nonline-mode=true\r\n")
	after, changed, err := replaceRCONPassword(raw, password)
	if err != nil || changed || !bytes.Equal(raw, after) {
		t.Fatal("matching escaped credential was rewritten")
	}
	continued := []byte("# keep\nrcon.pass\\\n  word = " + testEnvPassword[:10] + "\\\n  " + testEnvPassword[10:] + "\nother=true\n")
	after, changed, err = replaceRCONPassword(continued, testEnvPassword)
	if err != nil || changed || !bytes.Equal(continued, after) {
		t.Fatal("matching continued credential was rewritten")
	}
}

func TestReplaceRCONPasswordTouchesOnlyCredential(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(ending, "\r", "CR"), "\n", "LF"), func(t *testing.T) {
			prefix := "# preserve comments" + ending + "online-mode=true" + ending
			suffix := "motd=Original\\ value" + ending + "level-name=valuable-world"
			raw := []byte(prefix + "  rcon.password : old-test-only" + ending + suffix)
			got, changed, err := replaceRCONPassword(raw, testEnvPassword)
			want := prefix + "rcon.password=" + testEnvPassword + ending + suffix
			if err != nil || !changed || string(got) != want {
				t.Fatal("noncredential properties or line endings changed")
			}
		})
	}
	for _, raw := range []string{"", "online-mode=true", "online-mode=true\n"} {
		got, changed, err := replaceRCONPassword([]byte(raw), testEnvPassword)
		prefix := raw
		if prefix != "" && !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		if err != nil || !changed || string(got) != prefix+"rcon.password="+testEnvPassword+"\n" {
			t.Fatal("missing property was not appended without changing existing bytes")
		}
	}
	raw := []byte("other=true\nrcon.password=old-test-only")
	got, changed, err := replaceRCONPassword(raw, testEnvPassword)
	if err != nil || !changed || string(got) != "other=true\nrcon.password="+testEnvPassword {
		t.Fatal("unterminated final password line changed its ending")
	}
	raw = []byte("other=true\nrcon.password=old\\\n    continued\nafter=untouched\n")
	got, changed, err = replaceRCONPassword(raw, testEnvPassword)
	if err != nil || !changed || string(got) != "other=true\nrcon.password="+testEnvPassword+"\nafter=untouched\n" {
		t.Fatal("continued password update changed another property")
	}
}

func TestSyncPasswordAtomicPreservationAndNoop(t *testing.T) {
	cfg := temporaryLaunchConfig(t)
	path := filepath.Join(cfg.ServerDirectory, "server.properties")
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := syncServerPassword(path, testEnvPassword)
	if err != nil || !changed {
		t.Fatalf("password synchronization failed: %v", err)
	}
	updated, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, updated) {
		t.Fatal("password update was not an atomic replacement")
	}
	if original.Mode() != updated.Mode() {
		t.Fatal("properties permissions changed")
	}
	oldOwner, newOwner := original.Sys().(*syscall.Stat_t), updated.Sys().(*syscall.Stat_t)
	if oldOwner.Uid != newOwner.Uid || oldOwner.Gid != newOwner.Gid {
		t.Fatal("properties ownership changed")
	}
	want := "# keep exactly\nonline-mode=true\nrcon.password=" + testEnvPassword + "\nmotd=Original server\n"
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal("atomic synchronization changed unrelated bytes")
	}
	changed, err = syncServerPassword(path, testEnvPassword)
	if err != nil || changed {
		t.Fatal("matching password was rewritten")
	}
	final, err := os.Stat(path)
	if err != nil || !os.SameFile(updated, final) || !updated.ModTime().Equal(final.ModTime()) {
		t.Fatal("matching properties file metadata changed")
	}
	temps, err := filepath.Glob(filepath.Join(cfg.ServerDirectory, ".mcs-bootloader-properties-*"))
	if err != nil || len(temps) != 0 {
		t.Fatal("password synchronization left a temporary secret copy")
	}
}

func TestSyncPasswordRejectsUnsafeFilesWithoutChanges(t *testing.T) {
	secret := "TEST_ONLY_DO_NOT_ECHO_0123456789"
	for _, raw := range []string{
		"rcon.password=" + secret + "\nrcon.password=duplicate\n",
		"rcon.password=" + secret + `\uXXXX` + "\n",
		"rcon.password=" + secret + `\uD800` + "\n",
		"rcon.password=" + secret + "\xff\n",
	} {
		path := filepath.Join(t.TempDir(), "server.properties")
		if err := os.WriteFile(path, []byte(raw), 0640); err != nil {
			t.Fatal(err)
		}
		changed, err := syncServerPassword(path, testEnvPassword)
		if err == nil || changed {
			t.Fatal("unsafe properties accepted")
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testEnvPassword) {
			t.Fatal("properties error disclosed a secret")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != raw {
			t.Fatal("rejected properties were changed")
		}
	}
	directory := t.TempDir()
	target, link := filepath.Join(directory, "actual.properties"), filepath.Join(directory, "server.properties")
	raw := "rcon.password=" + secret + "\n"
	if err := os.WriteFile(target, []byte(raw), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if changed, err := syncServerPassword(link, testEnvPassword); err == nil || changed {
		t.Fatal("symlink properties accepted")
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != raw {
		t.Fatal("symlink target changed")
	}
}
