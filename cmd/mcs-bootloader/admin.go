package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/difof/mcs-bootloader/internal/backend"
	"github.com/difof/mcs-bootloader/internal/control"
	"github.com/difof/mcs-bootloader/internal/gateway"
)

const defaultConfigPath = "/etc/mcs-bootloader/.env"
const maxCommandTimeout = 5 * time.Minute

// A valid 64 KiB .env can expand when JSON escapes argument/message text
// (for example '<' becomes '\u003c'). Include that expansion and defaults
// while keeping both configuration response readers explicitly bounded.
const maxConfigResponseBytes = 512 * 1024
const maxMetricsResponseBytes = 8 * 1024 * 1024

type commandRequest struct {
	Command string `json:"command"`
	Timeout string `json:"timeout"`
}
type commandResponse struct {
	Reply string `json:"reply"`
}
type reloadRequest struct {
	ExpectedSHA256 string `json:"expected_sha256"`
	EnvPath        string `json:"env_path,omitempty"`
}

// This envelope is used only by backend-start/backend-stop on the same private
// socket. Ordinary status/config output never includes this credential field.
type backendEnvelope struct {
	Config   backend.Config `json:"config"`
	Password string         `json:"password"`
}

func jsonReply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}
func decodeRequest(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid request body")
	}
	return nil
}
func adminHandler(c *control.Controller, s *gateway.Server, m *runtimeConfig) http.Handler {
	mux := http.NewServeMux()
	snapshot := func(w http.ResponseWriter, r *http.Request) {
		_, metadata := m.snapshot()
		jsonReply(w, map[string]any{"version": version, "lifecycle": c.Snapshot(), "traffic": s.Stats(), "configuration": metadata})
	}
	mux.HandleFunc("GET /v1/status", snapshot)
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		cfg, metadata := m.snapshot()
		jsonReply(w, map[string]any{"configuration": metadata, "effective": cfg, "rcon_password": "[REDACTED]"})
	})
	mux.HandleFunc("GET /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		cfg, _ := m.snapshot()
		if !cfg.MetricsEnabled || m.metrics == nil {
			http.Error(w, "metrics are disabled; set MC_METRICS_ENABLED=true in .env and run task config:reload", http.StatusServiceUnavailable)
			return
		}
		// The same cache serves both endpoints; this route needs neither a
		// public listener address lookup nor any additional RCON exchange.
		forward := r.Clone(r.Context())
		forward.URL.Path = "/metrics"
		m.metrics.Handler().ServeHTTP(w, forward)
	})
	mux.HandleFunc("GET /v1/backend", func(w http.ResponseWriter, r *http.Request) {
		cfg, _ := m.snapshot()
		jsonReply(w, backendEnvelope{Config: cfg, Password: cfg.RCONPassword})
	})
	mux.HandleFunc("POST /v1/wake", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := c.Wake(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		snapshot(w, r)
	})
	mux.HandleFunc("POST /v1/sleep", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := c.Sleep(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		snapshot(w, r)
	})
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, r *http.Request) {
		var request reloadRequest
		if err := decodeRequest(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(request.ExpectedSHA256) != 64 {
			http.Error(w, "expected_sha256 is required", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		result, err := m.reload(ctx, request.ExpectedSHA256, request.EnvPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonReply(w, result)
	})
	mux.HandleFunc("POST /v1/restart-check", func(w http.ResponseWriter, r *http.Request) {
		var request reloadRequest
		if err := decodeRequest(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(request.ExpectedSHA256) != 64 {
			http.Error(w, "expected_sha256 is required", http.StatusBadRequest)
			return
		}
		result, err := m.restartCheck(request.ExpectedSHA256)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonReply(w, result)
	})
	mux.HandleFunc("POST /v1/command", func(w http.ResponseWriter, r *http.Request) {
		var request commandRequest
		if err := decodeRequest(w, r, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		command, err := operatorCommand([]string{request.Command})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		timeout, err := time.ParseDuration(request.Timeout)
		if err != nil || timeout <= 0 || timeout > maxCommandTimeout {
			http.Error(w, "command timeout must be positive and at most 5m", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		reply, err := m.command(ctx, command, timeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		jsonReply(w, commandResponse{Reply: reply})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && m.shuttingDown.Load() {
			http.Error(w, "gateway is shutting down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Each operation gets a fresh transport and is submitted once. In particular,
// command POSTs never gain an idempotency key or an application retry loop.
func socketRequest(ctx context.Context, socket, method, path string, body any, limit int64) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost/v1/"+path, payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("control response exceeds size limit")
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", response.Status, string(data))
	}
	return data, nil
}

func adminClient(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	socket := flags.String("socket", "/run/mcs-bootloader/control.sock", "admin Unix socket")
	configPath := flags.String("config", defaultConfigPath, "configuration file to apply")
	envPath := flags.String("env", "", "explicit .env source to reload (same file as the running daemon)")
	expected := flags.String("expected-sha256", "", "optional SHA-256 of the caller's validated .env snapshot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	method := "GET"
	if command != "status" && command != "config" && command != "metrics" {
		method = "POST"
	}
	var body any
	if command == "reload" || command == "restart-check" {
		requestPath := ""
		if *envPath != "" {
			var err error
			requestPath, err = filepath.Abs(*envPath)
			if err != nil {
				return err
			}
			*configPath = requestPath
		}
		_, digest, err := readConfigSnapshot(*configPath)
		if err != nil {
			return err
		}
		if *expected != "" {
			decoded, err := hex.DecodeString(*expected)
			if err != nil || len(decoded) != 32 {
				return errors.New("expected-sha256 must contain 64 hexadecimal characters")
			}
			if *expected != digest {
				return errors.New(".env changed after validation; prepare the request again")
			}
		}
		body = reloadRequest{ExpectedSHA256: digest, EnvPath: requestPath}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	limit := int64(65536)
	if command == "config" {
		limit = maxConfigResponseBytes
	}
	if command == "metrics" {
		limit = maxMetricsResponseBytes
	}
	data, err := socketRequest(ctx, *socket, method, command, body, limit)
	if err != nil {
		return err
	}
	var value any
	if json.Unmarshal(data, &value) == nil {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	_, err = os.Stdout.Write(data)
	return err
}

func appliedBackendConfig(socket string) (backend.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := socketRequest(ctx, socket, "GET", "backend", nil, maxConfigResponseBytes)
	if err != nil {
		return backend.Config{}, err
	}
	var envelope backendEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return backend.Config{}, errors.New("invalid applied backend configuration")
	}
	envelope.Config.RCONPassword = envelope.Password
	if err := envelope.Config.Validate(); err != nil {
		return backend.Config{}, err
	}
	if _, err := envelope.Config.Password(); err != nil {
		return backend.Config{}, err
	}
	return envelope.Config, nil
}
