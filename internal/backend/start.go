package backend

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
)

// StartJava replaces the backend helper with Java. It creates no shell or
// restart loop; the separately managed backend unit remains Java's owner.
func StartJava(cfg Config) error {
	launch, err := prepareJava(cfg)
	if err != nil {
		return err
	}
	defer launch.releasePorts()
	if _, err := syncServerPassword(launch.properties, launch.password); err != nil {
		return err
	}
	if err := os.Chdir(launch.directory); err != nil {
		return fmt.Errorf("cannot enter MC_SERVER_DIRECTORY")
	}
	// Retain exclusive bindings until every preflight and properties update is
	// complete. Java must acquire these same ports after exec.
	launch.releasePorts()
	if err := syscall.Exec(launch.binary, launch.args, os.Environ()); err != nil {
		return fmt.Errorf("cannot execute MC_JAVA_BINARY")
	}
	return nil
}

type javaLaunch struct {
	directory  string
	binary     string
	args       []string
	properties string
	password   string
	ports      []net.Listener
}

func (p *javaLaunch) releasePorts() {
	for _, listener := range p.ports {
		_ = listener.Close()
	}
	p.ports = nil
}

// prepareJava performs all checks without changing server.properties or the
// process working directory. Holding the returned listeners protects the
// narrow credential synchronization step against an already-running backend.
func prepareJava(cfg Config) (*javaLaunch, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	password, err := cfg.Password()
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(password) {
		return nil, fmt.Errorf("MC_RCON_PASSWORD must contain valid UTF-8")
	}
	stat, err := os.Stat(cfg.ServerDirectory)
	if err != nil || !stat.IsDir() {
		return nil, fmt.Errorf("MC_SERVER_DIRECTORY must be an existing directory")
	}
	stat, err = os.Stat(cfg.JavaBinary)
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("MC_JAVA_BINARY must be a regular executable file")
	}
	if err := validateJavaArgFile(cfg.ServerDirectory, cfg.JVMArgFile, "MC_JVM_ARGFILE"); err != nil {
		return nil, err
	}
	if err := validateJavaArgFile(cfg.ServerDirectory, cfg.ServerArgFile, "MC_SERVER_ARGFILE"); err != nil {
		return nil, err
	}
	launch := &javaLaunch{
		directory: cfg.ServerDirectory, binary: cfg.JavaBinary,
		args:       []string{cfg.JavaBinary, cfg.JVMArgFile, cfg.ServerArgFile, "nogui"},
		properties: filepath.Join(cfg.ServerDirectory, "server.properties"), password: password,
	}
	for _, port := range []struct{ name, address string }{
		{"backend", cfg.BackendAddress}, {"RCON", cfg.RCONAddress},
	} {
		listener, err := net.Listen("tcp", port.address)
		if err != nil {
			launch.releasePorts()
			return nil, fmt.Errorf("%s port is occupied or unavailable; refusing to change server.properties or start Java", port.name)
		}
		launch.ports = append(launch.ports, listener)
	}
	return launch, nil
}

func validateJavaArgFile(directory, argument, name string) error {
	if len(argument) < 2 || argument[0] != '@' || argument[1] == '@' || strings.ContainsAny(argument, "\x00\r\n") {
		return fmt.Errorf("%s must be a single @argument-file reference", name)
	}
	path := argument[1:]
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("%s must reference a readable regular file", name)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return fmt.Errorf("%s must reference a readable regular file", name)
	}
	return nil
}

const maxPropertiesBytes = 1024 * 1024

// syncServerPassword updates only the native rcon.password property. All other
// bytes, the original owner/group, and file permissions remain unchanged. The
// caller holds the private Minecraft/RCON port bindings until this completes.
func syncServerPassword(path, password string) (bool, error) {
	if err := validatePassword(password); err != nil {
		return false, err
	}
	if !utf8.ValidString(password) {
		return false, fmt.Errorf("MC_RCON_PASSWORD must contain valid UTF-8")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, fmt.Errorf("cannot open the regular server.properties file")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return false, fmt.Errorf("server.properties must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPropertiesBytes+1))
	if err != nil {
		return false, fmt.Errorf("cannot read server.properties")
	}
	if len(raw) > maxPropertiesBytes {
		return false, fmt.Errorf("server.properties exceeds 1 MiB")
	}
	updated, changed, err := replaceRCONPassword(raw, password)
	if err != nil || !changed {
		return false, err
	}
	directory := filepath.Dir(path)
	tmp, err := os.CreateTemp(directory, ".mc-gateway-properties-*")
	if err != nil {
		return false, fmt.Errorf("cannot create a private server.properties replacement")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()
	if original, ok := stat.Sys().(*syscall.Stat_t); ok {
		current, err := tmp.Stat()
		if err != nil {
			return false, fmt.Errorf("cannot inspect server.properties replacement")
		}
		owner, ok := current.Sys().(*syscall.Stat_t)
		if !ok {
			return false, fmt.Errorf("cannot verify server.properties ownership")
		}
		if owner.Uid != original.Uid || owner.Gid != original.Gid {
			if err := tmp.Chown(int(original.Uid), int(original.Gid)); err != nil {
				return false, fmt.Errorf("cannot preserve server.properties ownership")
			}
		}
	} else {
		return false, fmt.Errorf("cannot verify server.properties ownership")
	}
	if err := tmp.Chmod(stat.Mode()); err != nil {
		return false, fmt.Errorf("cannot preserve server.properties permissions")
	}
	if _, err := tmp.Write(updated); err != nil {
		return false, fmt.Errorf("cannot write server.properties replacement")
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("cannot sync server.properties replacement")
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("cannot close server.properties replacement")
	}
	// Abort if another writer replaced or edited the file during preparation.
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(stat, current) || current.Size() != stat.Size() || !current.ModTime().Equal(stat.ModTime()) {
		return false, fmt.Errorf("server.properties changed during preparation; retry after checking the backend")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("cannot atomically replace server.properties")
	}
	dir, err := os.Open(directory)
	if err != nil {
		return true, fmt.Errorf("server.properties was updated but its directory could not be opened for sync")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return true, fmt.Errorf("server.properties was updated but its directory could not be synced")
	}
	return true, nil
}

// replaceRCONPassword understands Java properties escapes and continuation
// lines, so a semantically matching credential is never rewritten. Duplicate
// credential entries are rejected instead of guessing which one was intended.
func replaceRCONPassword(raw []byte, password string) ([]byte, bool, error) {
	if !utf8.Valid(raw) {
		return nil, false, fmt.Errorf("server.properties must contain valid UTF-8")
	}
	type entry struct {
		start, end int
		ending     string
		value      string
	}
	var found *entry
	firstEnding := ""
	for pos := 0; pos < len(raw); {
		start := pos
		part, next, ending := propertyPhysicalLine(raw, pos)
		pos = next
		if firstEnding == "" && ending != "" {
			firstEnding = ending
		}
		trimmed := trimPropertySpace(part)
		if len(trimmed) == 0 || trimmed[0] == '#' || trimmed[0] == '!' {
			continue
		}
		logical := append([]byte(nil), part...)
		for propertyContinued(logical) && ending != "" {
			logical = logical[:len(logical)-1]
			if pos == len(raw) {
				break
			}
			part, next, ending = propertyPhysicalLine(raw, pos)
			pos = next
			logical = append(logical, trimPropertySpace(part)...)
		}
		keyRaw, valueRaw := splitJavaProperty(logical)
		key, err := decodeJavaProperty(keyRaw)
		if err != nil {
			return nil, false, fmt.Errorf("server.properties contains an invalid property name escape")
		}
		if key != "rcon.password" {
			continue
		}
		if found != nil {
			return nil, false, fmt.Errorf("server.properties contains duplicate rcon.password entries")
		}
		value, err := decodeJavaProperty(valueRaw)
		if err != nil {
			return nil, false, fmt.Errorf("server.properties contains an invalid rcon.password escape")
		}
		found = &entry{start: start, end: pos, ending: ending, value: value}
	}
	if found != nil && found.value == password {
		return raw, false, nil
	}
	line := []byte("rcon.password=" + encodeJavaProperty(password))
	if found != nil {
		updated := make([]byte, 0, len(raw)+len(line))
		updated = append(updated, raw[:found.start]...)
		updated = append(updated, line...)
		updated = append(updated, found.ending...)
		updated = append(updated, raw[found.end:]...)
		return updated, true, nil
	}
	if firstEnding == "" {
		firstEnding = "\n"
	}
	updated := append([]byte(nil), raw...)
	if len(updated) > 0 && updated[len(updated)-1] != '\n' && updated[len(updated)-1] != '\r' {
		updated = append(updated, firstEnding...)
	}
	updated = append(updated, line...)
	updated = append(updated, firstEnding...)
	return updated, true, nil
}

func propertyPhysicalLine(raw []byte, pos int) ([]byte, int, string) {
	start := pos
	for pos < len(raw) && raw[pos] != '\r' && raw[pos] != '\n' {
		pos++
	}
	part := raw[start:pos]
	if pos == len(raw) {
		return part, pos, ""
	}
	end := pos + 1
	if raw[pos] == '\r' && end < len(raw) && raw[end] == '\n' {
		end++
	}
	return part, end, string(raw[pos:end])
}

func trimPropertySpace(raw []byte) []byte { return bytes.TrimLeft(raw, " \t\f") }

func propertyContinued(raw []byte) bool {
	n := 0
	for i := len(raw) - 1; i >= 0 && raw[i] == '\\'; i-- {
		n++
	}
	return n%2 != 0
}

func propertySpace(c byte) bool { return c == ' ' || c == '\t' || c == '\f' }

func splitJavaProperty(raw []byte) ([]byte, []byte) {
	raw = trimPropertySpace(raw)
	keyEnd, valueStart := 0, len(raw)
	separator, escaped := false, false
	for keyEnd < len(raw) {
		c := raw[keyEnd]
		if !escaped && (c == '=' || c == ':') {
			valueStart = keyEnd + 1
			separator = true
			break
		}
		if !escaped && propertySpace(c) {
			valueStart = keyEnd + 1
			break
		}
		if c == '\\' {
			escaped = !escaped
		} else {
			escaped = false
		}
		keyEnd++
	}
	for valueStart < len(raw) {
		c := raw[valueStart]
		if !propertySpace(c) {
			if !separator && (c == '=' || c == ':') {
				separator = true
			} else {
				break
			}
		}
		valueStart++
	}
	return raw[:keyEnd], raw[valueStart:]
}

func decodeJavaProperty(raw []byte) (string, error) {
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			out.WriteByte(raw[i])
			continue
		}
		i++
		if i == len(raw) {
			return "", fmt.Errorf("unterminated property escape")
		}
		switch raw[i] {
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'n':
			out.WriteByte('\n')
		case 'f':
			out.WriteByte('\f')
		case 'u':
			if i+4 >= len(raw) {
				return "", fmt.Errorf("incomplete Unicode property escape")
			}
			n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				return "", fmt.Errorf("invalid Unicode property escape")
			}
			i += 4
			r := rune(n)
			if r >= 0xDC00 && r <= 0xDFFF {
				return "", fmt.Errorf("unpaired Unicode surrogate")
			}
			if r >= 0xD800 && r <= 0xDBFF {
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return "", fmt.Errorf("unpaired Unicode surrogate")
				}
				low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", fmt.Errorf("unpaired Unicode surrogate")
				}
				r = utf16.DecodeRune(r, rune(low))
				i += 6
			}
			out.WriteRune(r)
		default:
			out.WriteByte(raw[i])
		}
	}
	return out.String(), nil
}

func encodeJavaProperty(value string) string {
	var out strings.Builder
	for i, r := range value {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '\t':
			out.WriteString(`\t`)
		case '\r':
			out.WriteString(`\r`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case ' ':
			if i == 0 {
				out.WriteByte('\\')
			}
			out.WriteRune(r)
		default:
			if r >= 0x20 && r <= 0x7E {
				out.WriteRune(r)
			} else {
				for _, unit := range utf16.Encode([]rune{r}) {
					fmt.Fprintf(&out, `\u%04X`, unit)
				}
			}
		}
	}
	return out.String()
}
