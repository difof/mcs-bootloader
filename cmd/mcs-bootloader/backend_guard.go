package main

import (
	"encoding/hex"
	"errors"
	"os"
	"path"
	"strconv"
	"strings"
)

const backendHelperUsage = "backend helpers must run inside the configured non-root systemd backend service; use mcs-bootloader start or stop"

// The unit's User= and kernel cgroup membership are the authority for internal
// helpers. Do not use a pending .env edit to pick another account or trust an
// environment variable alone as evidence that systemd launched this process.
// Tests inject this value directly; production always reads the real process.
type backendServiceContext struct {
	EUID         int
	InvocationID string
	Cgroup       []byte
}

func currentBackendServiceContext() (backendServiceContext, error) {
	state := backendServiceContext{EUID: os.Geteuid(), InvocationID: os.Getenv("INVOCATION_ID")}
	if err := state.validateIdentity(); err != nil {
		return state, err
	}
	var err error
	state.Cgroup, err = readBoundedFile("/proc/self/cgroup", 64*1024)
	if err != nil {
		return state, errors.New(backendHelperUsage + "; cannot inspect the process cgroup")
	}
	return state, nil
}

func (s backendServiceContext) validateIdentity() error {
	if s.EUID <= 0 {
		return errors.New(backendHelperUsage + "; root execution is refused")
	}
	invocation, err := hex.DecodeString(s.InvocationID)
	if err != nil || len(invocation) != 16 || s.InvocationID == strings.Repeat("0", 32) {
		return errors.New(backendHelperUsage + "; no valid systemd invocation")
	}
	return nil
}

func (s backendServiceContext) validateService(service string) error {
	if err := s.validateIdentity(); err != nil {
		return err
	}
	group, err := systemdCgroup(s.Cgroup)
	if err != nil || !backendCgroupMatches(group, service) {
		return errors.New(backendHelperUsage + "; process is outside its configured backend unit")
	}
	return nil
}

// Legacy/hybrid hosts place systemd membership in name=systemd. The unified
// hierarchy is authoritative when that legacy hierarchy is absent. A matching
// CPU/memory-controller path alone must never authorize a helper.
func systemdCgroup(data []byte) (string, error) {
	if len(data) == 0 || len(data) > 64*1024 || strings.ContainsAny(string(data), "\x00\r") {
		return "", errors.New("invalid cgroup metadata")
	}
	var unified, legacy string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			return "", errors.New("invalid cgroup record")
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil || id < 0 || !strings.HasPrefix(fields[2], "/") || path.Clean(fields[2]) != fields[2] {
			return "", errors.New("invalid cgroup record")
		}
		if id == 0 && fields[1] == "" {
			if unified != "" {
				return "", errors.New("duplicate unified cgroup")
			}
			unified = fields[2]
		}
		for _, controller := range strings.Split(fields[1], ",") {
			if controller == "name=systemd" {
				if id == 0 || legacy != "" {
					return "", errors.New("invalid legacy systemd cgroup")
				}
				legacy = fields[2]
			}
		}
	}
	if legacy != "" {
		return legacy, nil
	}
	if unified != "" {
		return unified, nil
	}
	return "", errors.New("systemd cgroup hierarchy is unavailable")
}

// System services are nested in zero or more slices. Match a complete unit
// component, allowing systemd's .control subgroup for ExecStop and delegated
// subgroups, but not a similarly named unit or another service's descendants.
func backendCgroupMatches(group, service string) bool {
	if !strings.HasSuffix(service, ".service") || strings.ContainsAny(service, "/\x00\r\n") {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(group, "/"), "/") {
		// systemd prefixes one underscore when a unit name could collide with
		// a kernel cgroup attribute (for example cpu.service).
		component = strings.TrimPrefix(component, "_")
		if component == service {
			return true
		}
		if !strings.HasSuffix(component, ".slice") {
			return false
		}
	}
	return false
}
