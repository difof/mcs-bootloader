package operator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
}

func TestStandaloneInitAndShowWithIsolatedPython(t *testing.T) {
	requirePython(t)
	root := t.TempDir()
	poison := filepath.Join(root, "python-poison")
	if err := os.Mkdir(poison, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sitecustomize.py", "env_config.py", "manage.py"} {
		if err := os.WriteFile(filepath.Join(poison, name), []byte("raise RuntimeError('UNTRUSTED_MODULE_EXECUTED')\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PYTHONPATH", poison)
	t.Setenv("PYTHONHOME", poison)
	path := filepath.Join(root, "literal {{.CONFIG}} $() .env")
	opts := Options{Env: path}
	raw, err := Run(context.Background(), "init", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result["ok"] != true {
		t.Fatalf("invalid init result: %s", raw)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private config mode: %v %v", info, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "replace-with-a-random-private-password") {
		t.Fatal("placeholder was not replaced")
	}
	before := string(content)
	if _, err = Run(context.Background(), "init", nil, opts); err == nil {
		t.Fatal("init overwrote existing config")
	}
	content, _ = os.ReadFile(path)
	if string(content) != before {
		t.Fatal("existing config changed")
	}
	raw, err = Run(context.Background(), "config-show", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &result) != nil {
		t.Fatal("show is not JSON")
	}
	settings := result["settings"].(map[string]any)
	if settings["MC_RCON_PASSWORD"] != "[REDACTED]" {
		t.Fatal("password was not redacted")
	}
	if settings["MC_SERVER_DIRECTORY"] != "/srv/minecraft" {
		t.Fatal("embedded example unavailable")
	}
}

func TestEmbeddedResourcesArePrivateAndComplete(t *testing.T) {
	root := t.TempDir()
	if err := extract(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env.example", "scripts/manage.py", "scripts/remove.py", "scripts/install.py", "scripts/env_config.py", "scripts/service_config.py", "deploy/mcs-bootloader.service", "deploy/mcs-bootloader-backend.service", "deploy/journald@mcs-bootloader.conf"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("invalid asset %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "scripts/test_install.py")); !os.IsNotExist(err) {
		t.Fatal("tests must not be embedded")
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !os.IsNotExist(err) {
		t.Fatal("real config must not be embedded")
	}
}

func TestOperatorEditHelper(t *testing.T) {
	path := os.Getenv("MCS_OPERATOR_EDIT_HELPER")
	if path == "" {
		t.Skip("PTY subprocess helper")
	}
	data, err := Run(context.Background(), "config-edit", nil, Options{Env: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestEditKeepsEditorInForegroundTerminal(t *testing.T) {
	requirePython(t)
	root := t.TempDir()
	path := filepath.Join(root, "test.env")
	if err := os.WriteFile(path, []byte("# private test config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(root, "editor")
	script := "#!/bin/sh\nprintf 'EDITOR_READY\\n'\nIFS= read -r line\nprintf '%s\\n' \"$line\" >> \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// pty.fork supplies a controlling terminal and foreground process group.
	// The editor must actually read it; merely opening /dev/tty is insufficient
	// to detect SIGTTIN caused by accidentally creating a background group.
	harness := `import errno,os,pty,select,signal,sys,time
pid,fd=pty.fork()
if pid==0:
 os.environ["MCS_OPERATOR_EDIT_HELPER"]=sys.argv[2]
 os.environ["EDITOR"]=sys.argv[3]
 os.environ["VISUAL"]=""
 os.execl(sys.argv[1],sys.argv[1],"-test.run=^TestOperatorEditHelper$")
data=b""; sent=False; deadline=time.monotonic()+10
while time.monotonic()<deadline:
 ready,_,_=select.select([fd],[],[],0.1)
 if ready:
  try: chunk=os.read(fd,65536)
  except OSError as e:
   if e.errno!=errno.EIO: raise
   chunk=b""
  if not chunk: break
  data+=chunk
  if b"EDITOR_READY" in data and not sent:
   os.write(fd,b"MC_IDLE_TIMEOUT=2h\n"); sent=True
 done,status=os.waitpid(pid,os.WNOHANG)
 if done:
  sys.stdout.buffer.write(data)
  sys.exit(os.waitstatus_to_exitcode(status) if sent else 2)
else:
 os.killpg(pid,signal.SIGKILL)
 os.waitpid(pid,0)
 raise RuntimeError("editor could not read its foreground terminal")
_,status=os.waitpid(pid,0)
sys.stdout.buffer.write(data)
sys.exit(os.waitstatus_to_exitcode(status) if sent else 2)
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-I", "-S", "-c", harness, executable, path, editor)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("interactive editor: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "MC_IDLE_TIMEOUT=2h\n") {
		t.Fatalf("editor did not save input: %v %q", err, data)
	}
}
