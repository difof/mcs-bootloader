package whitelist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testUUID = "a9a9891b-906e-4abd-9ad1-dfcfc2b7eb8e"

func writePolicy(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func listFor(names ...string) string {
	entries := make([]map[string]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, map[string]string{"uuid": testUUID, "name": name})
	}
	raw, _ := json.Marshal(entries)
	return string(raw)
}

func TestDisabledWhitelistDoesNotReadList(t *testing.T) {
	directory := t.TempDir()
	writePolicy(t, directory, "server.properties", "white-list=false\n")
	for _, list := range []string{"", "corrupt", `null`} {
		if list != "" {
			writePolicy(t, directory, "whitelist.json", list)
		}
		if allowed, err := Check(directory, "AnyPlayer"); err != nil || !allowed {
			t.Fatalf("disabled whitelist unexpectedly checked list: allowed=%v err=%v", allowed, err)
		}
		if state, err := Snapshot(directory); err != nil || !state.Known || state.Enabled || len(state.Entries) != 0 {
			t.Fatalf("disabled snapshot unexpectedly read list: state=%+v err=%v", state, err)
		}
	}
}

func TestSnapshotReflectsNativeEntriesWithoutKeepingStaleState(t *testing.T) {
	directory := t.TempDir()
	writePolicy(t, directory, "server.properties", "white-list=true\n")
	writePolicy(t, directory, "whitelist.json", listFor("Alice", "Steve"))
	state, err := Snapshot(directory)
	if err != nil || !state.Known || !state.Enabled || len(state.Entries) != 2 {
		t.Fatalf("enabled snapshot = %+v, %v", state, err)
	}
	if state.Entries[0] != (Entry{Name: "Alice", UUID: testUUID}) || state.Entries[1].Name != "Steve" {
		t.Fatalf("snapshot lost native entry details: %+v", state.Entries)
	}
	writePolicy(t, directory, "whitelist.json", listFor("Alex"))
	state, err = Snapshot(directory)
	if err != nil || !state.Known || !state.Enabled || len(state.Entries) != 1 || state.Entries[0].Name != "Alex" {
		t.Fatalf("snapshot kept a stale native list: %+v, %v", state, err)
	}
	// A valid entry followed by a broken tail must not become a partial metrics
	// list or retain the preceding successful snapshot.
	writePolicy(t, directory, "whitelist.json", strings.TrimSuffix(listFor("Alex"), "]")+",")
	state, err = Snapshot(directory)
	if err == nil || state.Known || state.Enabled || len(state.Entries) != 0 {
		t.Fatalf("broken policy exposed a partial/stale snapshot: %+v, %v", state, err)
	}
	writePolicy(t, directory, "server.properties", "white-list=false\n")
	state, err = Snapshot(directory)
	if err != nil || !state.Known || state.Enabled || len(state.Entries) != 0 {
		t.Fatalf("disabled snapshot retained enabled entries: %+v, %v", state, err)
	}
}

func TestEveryLoginSeesNativeEditsAndToggle(t *testing.T) {
	directory := t.TempDir()
	writePolicy(t, directory, "server.properties", "white-list=true\n")
	writePolicy(t, directory, "whitelist.json", listFor("Alice"))
	check := func(name string, want bool) {
		t.Helper()
		if got, err := Check(directory, name); err != nil || got != want {
			t.Fatalf("Check(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	check("ALICE", true)
	check("Steve", false)
	// Native add/remove writes and external file+whitelist reload changes are
	// visible on the next login. Even equal length/mtime must not hide edits.
	path := filepath.Join(directory, "whitelist.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writePolicy(t, directory, "whitelist.next", listFor("Steve"))
	if err := os.Chtimes(filepath.Join(directory, "whitelist.next"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, "whitelist.next"), path); err != nil {
		t.Fatal(err)
	}
	check("Alice", false)
	check("Steve", true)
	writePolicy(t, directory, "server.properties", "white-list=false\n")
	check("Stranger", true)
	writePolicy(t, directory, "server.properties", "white-list=true\n")
	check("Stranger", false)
	writePolicy(t, directory, "whitelist.json", "[]")
	check("Steve", false)
}

func TestUnavailableOrPartialPolicyFailsClosed(t *testing.T) {
	cases := []struct{ name, properties, list string }{
		{"missing properties", "", listFor("Alice")},
		{"missing flag", "server-port=25566\n", listFor("Alice")},
		{"partial flag", "white-list=tru", listFor("Alice")},
		{"invalid flag", "white-list=unknown\n", listFor("Alice")},
		{"missing list", "white-list=true\n", ""},
		{"null list", "white-list=true\n", "null"},
		{"object list", "white-list=true\n", "{}"},
		{"partial tail", "white-list=true\n", strings.TrimSuffix(listFor("Alice"), "]") + ","},
		{"trailing value", "white-list=true\n", listFor("Alice") + "[]"},
		{"missing UUID", "white-list=true\n", `[{"name":"Alice"}]`},
		{"invalid UUID", "white-list=true\n", `[{"name":"Alice","uuid":"invalid"}]`},
		{"name case key", "white-list=true\n", `[{"Name":"Alice","uuid":"` + testUUID + `"}]`},
		{"null entry", "white-list=true\n", `[null]`},
		{"bad unicode", "white-list=true\n", `[{"name":"Ali\ud800ce","uuid":"` + testUUID + `"}]`},
		{"control name", "white-list=true\n", `[{"name":"Ali\nce","uuid":"` + testUUID + `"}]`},
		{"bad UTF8", "white-list=true\n", "[\xff]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			if tc.properties != "" {
				writePolicy(t, directory, "server.properties", tc.properties)
			}
			if tc.list != "" {
				writePolicy(t, directory, "whitelist.json", tc.list)
			}
			if allowed, err := Check(directory, "Alice"); allowed || err == nil {
				t.Fatalf("unreadable/malformed policy allowed=%v err=%v", allowed, err)
			}
			if state, err := Snapshot(directory); err == nil || state.Known || state.Enabled || len(state.Entries) != 0 {
				t.Fatalf("unreadable/malformed policy snapshot=%+v err=%v", state, err)
			}
		})
	}
}

func TestPolicyFileBoundsAndTypes(t *testing.T) {
	for _, filename := range []string{"server.properties", "whitelist.json"} {
		for _, kind := range []string{"oversized", "directory", "fifo"} {
			t.Run(filename+"/"+kind, func(t *testing.T) {
				directory := t.TempDir()
				writePolicy(t, directory, "server.properties", "white-list=true\n")
				writePolicy(t, directory, "whitelist.json", listFor("Alice"))
				path := filepath.Join(directory, filename)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "oversized":
					writePolicy(t, directory, filename, strings.Repeat("x", maxFileBytes+1))
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
				started := time.Now()
				if allowed, err := Check(directory, "Alice"); allowed || err == nil {
					t.Fatalf("nonregular/oversized file allowed=%v err=%v", allowed, err)
				}
				if time.Since(started) > time.Second {
					t.Fatal("policy read blocked on a special file")
				}
			})
		}
	}
}

func TestJavaPropertySyntaxAndConservativePartialReadHandling(t *testing.T) {
	for _, raw := range []string{
		"white-list=true\n", "white-list: true\r\n", "white-list\tTRUE\r",
		"! comment\n # comment\nwhite-list = true\n",
		"white\\-list=tr\\\n  ue\n", "white\\u002dlist=\\u0074rue\n",
		"white-list=false\nwhite-list=true\n",
		"motd=unrelated\\\\\nwhite-list=true\n",
	} {
		if enabled, err := enabledProperty([]byte(raw)); err != nil || !enabled {
			t.Fatalf("valid native property syntax enabled=%v err=%v for %q", enabled, err, raw)
		}
	}
	for _, raw := range []string{"white-list=false\n", "white-list=true\nwhite-list=FALSE\n"} {
		if enabled, err := enabledProperty([]byte(raw)); err != nil || enabled {
			t.Fatalf("disabled property enabled=%v err=%v", enabled, err)
		}
	}
	for _, raw := range []string{"white-list=true ", "white-list=tr\\", "white-list=tr\\\n", "white-list=\\u00xx", "white\\uD800-list=true", "white-list=true\nmotd=incomplete\\"} {
		if _, err := enabledProperty([]byte(raw)); err == nil {
			t.Fatalf("accepted ambiguous/partial native properties %q", raw)
		}
	}
}

func TestReplacementCannotMatchPreviousSnapshot(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "whitelist.json")
	writePolicy(t, directory, "whitelist.json", listFor("Alice"))
	_, before, err := readStable(path)
	if err != nil {
		t.Fatal(err)
	}
	writePolicy(t, directory, "next.json", listFor("Steve"))
	if err := os.Rename(filepath.Join(directory, "next.json"), path); err != nil {
		t.Fatal(err)
	}
	if unchanged(path, before) {
		t.Fatal("atomic replacement looked like the earlier policy snapshot")
	}
}
