package whitelist

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// enabledProperty understands Java Properties separators, escapes, CR/LF, and
// continuation lines. Minecraft writes an explicit white-list key; requiring
// it prevents a temporarily truncated properties file from disabling the gate.
func enabledProperty(raw []byte) (bool, error) {
	invalid := errors.New("server.properties needs a valid explicit white-list=true or white-list=false setting")
	if !utf8.Valid(raw) {
		return false, invalid
	}
	found, enabled := false, false
	for pos := 0; pos < len(raw); {
		part, next, ending := physicalLine(raw, pos)
		pos = next
		trimmed := trimSpace(part)
		if len(trimmed) == 0 || trimmed[0] == '#' || trimmed[0] == '!' {
			continue
		}
		logical := append([]byte(nil), part...)
		for continued(logical) {
			if !ending || pos == len(raw) {
				return false, invalid
			}
			logical = logical[:len(logical)-1]
			part, next, ending = physicalLine(raw, pos)
			pos = next
			logical = append(logical, trimSpace(part)...)
		}
		keyRaw, valueRaw := splitProperty(logical)
		key, err := decodeProperty(keyRaw)
		if err != nil {
			return false, invalid
		}
		if key != "white-list" {
			continue
		}
		value, err := decodeProperty(valueRaw)
		if err != nil {
			return false, invalid
		}
		switch {
		case strings.EqualFold(value, "true"):
			enabled = true
		case strings.EqualFold(value, "false"):
			enabled = false
		default:
			return false, invalid
		}
		// Java Properties keeps the final assignment if a key is repeated.
		found = true
	}
	if !found {
		return false, invalid
	}
	return enabled, nil
}

func physicalLine(raw []byte, pos int) ([]byte, int, bool) {
	start := pos
	for pos < len(raw) && raw[pos] != '\r' && raw[pos] != '\n' {
		pos++
	}
	part := raw[start:pos]
	if pos == len(raw) {
		return part, pos, false
	}
	end := pos + 1
	if raw[pos] == '\r' && end < len(raw) && raw[end] == '\n' {
		end++
	}
	return part, end, true
}

func trimSpace(raw []byte) []byte { return bytes.TrimLeft(raw, " \t\f") }
func isSpace(char byte) bool      { return char == ' ' || char == '\t' || char == '\f' }

func continued(raw []byte) bool {
	n := 0
	for i := len(raw) - 1; i >= 0 && raw[i] == '\\'; i-- {
		n++
	}
	return n%2 != 0
}

func splitProperty(raw []byte) ([]byte, []byte) {
	raw = trimSpace(raw)
	keyEnd, valueStart := 0, len(raw)
	separator, escaped := false, false
	for keyEnd < len(raw) {
		char := raw[keyEnd]
		if !escaped && (char == '=' || char == ':') {
			valueStart = keyEnd + 1
			separator = true
			break
		}
		if !escaped && isSpace(char) {
			valueStart = keyEnd + 1
			break
		}
		if char == '\\' {
			escaped = !escaped
		} else {
			escaped = false
		}
		keyEnd++
	}
	for valueStart < len(raw) {
		char := raw[valueStart]
		if !isSpace(char) {
			if !separator && (char == '=' || char == ':') {
				separator = true
			} else {
				break
			}
		}
		valueStart++
	}
	return raw[:keyEnd], raw[valueStart:]
}

func decodeProperty(raw []byte) (string, error) {
	invalid := errors.New("invalid Java property escape")
	var result strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			result.WriteByte(raw[i])
			continue
		}
		i++
		if i == len(raw) {
			return "", invalid
		}
		switch raw[i] {
		case 't':
			result.WriteByte('\t')
		case 'r':
			result.WriteByte('\r')
		case 'n':
			result.WriteByte('\n')
		case 'f':
			result.WriteByte('\f')
		case 'u':
			if i+4 >= len(raw) {
				return "", invalid
			}
			n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				return "", invalid
			}
			i += 4
			char := rune(n)
			if char >= 0xDC00 && char <= 0xDFFF {
				return "", invalid
			}
			if char >= 0xD800 && char <= 0xDBFF {
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return "", invalid
				}
				low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", invalid
				}
				char = utf16.DecodeRune(char, rune(low))
				i += 6
			}
			result.WriteRune(char)
		default:
			result.WriteByte(raw[i])
		}
	}
	return result.String(), nil
}
