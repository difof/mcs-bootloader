package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func fixtureBackendServiceContext(service string) backendServiceContext {
	return backendServiceContext{
		EUID: 1000, InvocationID: "0123456789abcdef0123456789abcdef",
		Cgroup: []byte("0::/system.slice/" + service + "\n"),
	}
}

func TestBackendServiceContextRequiresRealServiceBoundary(t *testing.T) {
	const unit = "mc-gateway-backend.service"
	for _, group := range []string{
		"0::/system.slice/" + unit + "\n",
		"0::/" + unit + "\n",
		"0::/games.slice/games-minecraft.slice/" + unit + "/.control\n",
		"5:cpu,memory:/arbitrary\n1:name=systemd:/system.slice/" + unit + "\n",
		"0::/\n1:name=systemd:/system.slice/" + unit + "\n",
	} {
		service := fixtureBackendServiceContext(unit)
		service.Cgroup = []byte(group)
		if err := service.validateService(unit); err != nil {
			t.Fatalf("valid backend service rejected: %q: %v", group, err)
		}
	}
	escaped := fixtureBackendServiceContext("_cpu.service")
	if err := escaped.validateService("cpu.service"); err != nil {
		t.Fatalf("systemd-escaped unit rejected: %v", err)
	}
	cases := map[string]func(*backendServiceContext){
		"root":             func(s *backendServiceContext) { s.EUID = 0 },
		"negative UID":     func(s *backendServiceContext) { s.EUID = -1 },
		"no invocation":    func(s *backendServiceContext) { s.InvocationID = "" },
		"short invocation": func(s *backendServiceContext) { s.InvocationID = "abc" },
		"invalid hex":      func(s *backendServiceContext) { s.InvocationID = strings.Repeat("z", 32) },
		"zero invocation":  func(s *backendServiceContext) { s.InvocationID = strings.Repeat("0", 32) },
		"manual session": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/user.slice/user-1000.slice/session-1.scope\n")
		},
		"wrong unit": func(s *backendServiceContext) { s.Cgroup = []byte("0::/system.slice/other.service\n") },
		"suffix unit": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/system.slice/prefix-" + unit + "\n")
		},
		"prefix unit": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/system.slice/" + unit + "-extra\n")
		},
		"child of wrong service": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/system.slice/other.service/" + unit + "\n")
		},
		"user unit": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + unit + "\n")
		},
		"matching unrelated controller": func(s *backendServiceContext) {
			s.Cgroup = []byte("5:cpu,memory:/system.slice/" + unit + "\n1:name=systemd:/system.slice/other.service\n")
		},
		"hybrid legacy takes precedence": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/system.slice/" + unit + "\n1:name=systemd:/system.slice/other.service\n")
		},
		"missing":            func(s *backendServiceContext) { s.Cgroup = nil },
		"malformed":          func(s *backendServiceContext) { s.Cgroup = []byte("not a cgroup\n") },
		"no systemd tree":    func(s *backendServiceContext) { s.Cgroup = []byte("2:cpu:/system.slice/" + unit + "\n") },
		"duplicate v2":       func(s *backendServiceContext) { s.Cgroup = append(s.Cgroup, s.Cgroup...) },
		"oversized metadata": func(s *backendServiceContext) { s.Cgroup = []byte(strings.Repeat("x", 65537)) },
		"parent traversal": func(s *backendServiceContext) {
			s.Cgroup = []byte("0::/system.slice/other.service/../" + unit + "\n")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			service := fixtureBackendServiceContext(unit)
			mutate(&service)
			if err := service.validateService(unit); err == nil || !strings.Contains(err.Error(), "use mcs-bootloader start or stop") {
				t.Fatalf("unsafe service context accepted or missing operator guidance: %v", err)
			}
		})
	}
}

func TestBackendHelperRejectsManualInvocationAndStillProvidesHelp(t *testing.T) {
	// A shell cannot become an authorized service by reaching the private socket.
	// No socket/config reads are needed to reject an absent systemd invocation.
	t.Setenv("INVOCATION_ID", "")
	for _, command := range []string{"backend-start", "backend-stop"} {
		var stdout, stderr bytes.Buffer
		out := newCLIOutput(&stdout, &stderr)
		err := executeCLI(context.Background(), []string{command, "--json"}, out)
		if err == nil || !strings.Contains(err.Error(), "use mcs-bootloader start or stop") {
			t.Fatalf("manual helper accepted: %v", err)
		}
		out.Error(err)
		if stdout.Len() != 0 || !json.Valid(stderr.Bytes()) || strings.Contains(stderr.String(), "\x1b") {
			t.Fatal("helper rejection was not clean JSON")
		}
		stdout.Reset()
		if err := executeCLI(context.Background(), []string{command, "--help", "--json"}, newCLIOutput(&stdout, io.Discard)); err != nil || !json.Valid(stdout.Bytes()) {
			t.Fatalf("helper help needs service privileges: %v", err)
		}
	}
}

func TestBackendHelperChecksAppliedUnitBeforeStop(t *testing.T) {
	f := newRuntimeFixture(t, true)
	startRuntimeSocket(t, f)
	service := fixtureBackendServiceContext("different.service")
	// Invalid PID prevents RCON even if a future regression removes the guard.
	err := runBackendInService(cliOptions{Command: "backend-stop", Socket: f.socket, Env: f.m.path, PID: "1"}, service)
	if err == nil || !strings.Contains(err.Error(), "outside its configured backend unit") {
		t.Fatalf("helper did not verify the applied backend service: %v", err)
	}
}
