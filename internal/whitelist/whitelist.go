// Package whitelist reads Minecraft's own admission settings. It never writes
// server files, keeps a second whitelist, or authenticates a player's identity.
package whitelist

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// These are parser safety bounds, not configurable admission policy. Minecraft
// owns the filenames, enabled setting, and list contents.
const maxFileBytes = 1 << 20

// Entry is a validated native whitelist entry. Its name is a claimed profile
// name, not proof of authentication; Minecraft owns the final UUID decision.
type Entry struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

// State is a freshly read native whitelist snapshot. Known is false on any
// read/validation error; Entries is populated only when Enabled is true.
type State struct {
	Known   bool    `json:"known"`
	Enabled bool    `json:"enabled"`
	Entries []Entry `json:"entries"`
}

// Check evaluates a claimed login name against the latest native files.
// false,nil means the name is absent; an error means policy could not safely be
// read and callers must deny admission. Minecraft still performs its own UUID,
// authentication, and whitelist checks after a connection is forwarded.
//
// A fresh read on every login observes in-game whitelist on/off/add/remove and
// reload without a gateway reload. No player list or enabled flag is cached.
func Check(serverDirectory, claimedName string) (bool, error) {
	if !validName(claimedName) {
		return false, nil
	}
	allowed := false
	enabled, err := readPolicy(serverDirectory, func(entry Entry) {
		if strings.EqualFold(entry.Name, claimedName) {
			allowed = true
		}
	})
	return err == nil && (!enabled || allowed), err
}

// Snapshot returns the current native policy for status/metrics without a
// cached list or filesystem changes. Disabled policy never opens whitelist.json.
// Errors return no entries, even if some entries were valid before a bad tail.
func Snapshot(serverDirectory string) (State, error) {
	entries := make([]Entry, 0)
	enabled, err := readPolicy(serverDirectory, func(entry Entry) {
		entries = append(entries, entry)
	})
	if err != nil {
		return State{}, err
	}
	return State{Known: true, Enabled: enabled, Entries: entries}, nil
}

func readPolicy(serverDirectory string, visit func(Entry)) (bool, error) {
	if !filepath.IsAbs(serverDirectory) {
		return false, errors.New("Minecraft directory must be absolute for whitelist checks")
	}
	propertiesPath := filepath.Join(serverDirectory, "server.properties")
	properties, propsInfo, err := readStable(propertiesPath)
	if err != nil {
		return false, errors.New("cannot read stable server.properties for whitelist policy")
	}
	enabled, err := enabledProperty(properties)
	if err != nil {
		return false, err
	}
	if !enabled {
		if !unchanged(propertiesPath, propsInfo) {
			return false, errors.New("Minecraft whitelist files changed during the policy read; retry")
		}
		return false, nil
	}
	listPath := filepath.Join(serverDirectory, "whitelist.json")
	list, listInfo, err := readStable(listPath)
	if err != nil {
		return false, errors.New("cannot read stable whitelist.json while the whitelist is enabled")
	}
	if err := readEntries(list, visit); err != nil {
		return false, err
	}
	// A native command/editor may replace either file while the other is read.
	// Reject that mixed snapshot instead of trusting an old enabled flag/list.
	if !unchanged(propertiesPath, propsInfo) || !unchanged(listPath, listInfo) {
		return false, errors.New("Minecraft whitelist files changed during the policy read; retry")
	}
	return true, nil
}

func readStable(path string) ([]byte, os.FileInfo, error) {
	// Nonblocking open lets us reject a FIFO/device without hanging admission.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxFileBytes {
		return nil, nil, errors.New("policy file must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(raw) > maxFileBytes {
		return nil, nil, errors.New("policy file cannot be read within its size limit")
	}
	after, err := f.Stat()
	if err != nil || !sameVersion(before, after) || int64(len(raw)) != after.Size() || !unchanged(path, after) {
		return nil, nil, errors.New("policy file changed while being read")
	}
	return raw, after, nil
}

func sameVersion(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) && before.Mode() == after.Mode()
}

func unchanged(path string, before os.FileInfo) bool {
	after, err := os.Stat(path)
	return err == nil && sameVersion(before, after)
}

func readEntries(raw []byte, visit func(Entry)) error {
	invalid := errors.New("whitelist.json must contain valid player entries")
	if !utf8.Valid(raw) {
		return invalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('[') {
		return invalid
	}
	for d.More() {
		var entry map[string]json.RawMessage
		if err := d.Decode(&entry); err != nil || entry == nil {
			return invalid
		}
		var name, uuid string
		if json.Unmarshal(entry["name"], &name) != nil || !validName(name) ||
			json.Unmarshal(entry["uuid"], &uuid) != nil || !validUUID(uuid) {
			return invalid
		}
		visit(Entry{Name: name, UUID: uuid})
	}
	if last, err := d.Token(); err != nil || last != json.Delim(']') {
		return invalid
	}
	if d.Decode(new(any)) != io.EOF {
		return invalid
	}
	// Validate the entire file before returning a match: a truncated tail must
	// never allow a player merely because their entry appeared first.
	return nil
}

func validName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 16 {
		return false
	}
	for _, char := range name {
		if char == utf8.RuneError || unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range []byte(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
		} else if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}
