package backend

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// EnvValues returns the literal, explicitly configured values after validating
// the complete runtime configuration. It shares the daemon's parser and never
// expands shell text. The returned map is private: callers must redact secrets.
func EnvValues(data []byte) (map[string]string, error) {
	if _, err := LoadEnv(data); err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.Trim(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[6:], " \t")
		}
		parts := envAssignment.FindStringSubmatch(line)
		value, err := parseEnvValue(parts[2])
		if err != nil {
			return nil, err
		}
		values[parts[1]] = value
	}
	return values, nil
}

// OperatorValues reads only the small set of keys needed to reach an already
// running installation. A bad pending timer/MOTD/metrics edit must not prevent
// status, logs, or a safe stop using the daemon's still-valid applied settings.
// It never accepts routing keys with malformed literals or duplicate entries.
func OperatorValues(data []byte) (map[string]string, error) {
	if len(data) > maxEnvBytes {
		return nil, fmt.Errorf("environment file exceeds 64 KiB")
	}
	if !utf8.Valid(data) || bytes.IndexAny(data, "\x00\r") >= 0 {
		return nil, fmt.Errorf("environment file must be UTF-8 with LF line endings and no NUL")
	}
	keys := map[string]bool{"MC_ADMIN_SOCKET": true, "MC_GATEWAY_SERVICE": true, "MC_BACKEND_SERVICE": true, "MC_LOG_NAMESPACE": true, "MC_LOG_LINES": true, "MC_COMMAND_TIMEOUT": true, "MC_SERVICE_USER": true, "MC_RCON_PASSWORD": true}
	values := make(map[string]string)
	for lineIndex, line := range strings.Split(string(data), "\n") {
		line = strings.Trim(line, " \t")
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[6:], " \t")
		}
		parts := envAssignment.FindStringSubmatch(line)
		if parts == nil || !keys[parts[1]] {
			continue
		}
		key := parts[1]
		if _, seen := values[key]; seen {
			return nil, envError(lineIndex+1, key, "duplicate setting")
		}
		value, err := parseEnvValue(parts[2])
		if err != nil {
			return nil, envError(lineIndex+1, key, err.Error())
		}
		if err := validateEnvString(key, value); err != nil {
			return nil, envError(lineIndex+1, key, err.Error())
		}
		values[key] = value
	}
	return values, nil
}
