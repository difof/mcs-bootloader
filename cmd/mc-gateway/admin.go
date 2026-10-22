package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"mc-gateway/internal/control"
	"mc-gateway/internal/gateway"
)

func adminHandler(c *control.Controller, s *gateway.Server) http.Handler {
	mux := http.NewServeMux()
	snapshot := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": version, "lifecycle": c.Snapshot(), "traffic": s.Stats()})
	}
	mux.HandleFunc("GET /v1/status", snapshot)
	mux.HandleFunc("POST /v1/wake", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := c.Wake(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		snapshot(w, r)
	})
	mux.HandleFunc("POST /v1/sleep", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := c.Sleep(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		snapshot(w, r)
	})
	return mux
}

func adminClient(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	socket := flags.String("socket", "/run/mc-gateway/control.sock", "admin Unix socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", *socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	method := "GET"
	if command != "status" {
		method = "POST"
	}
	request, err := http.NewRequest(method, "http://localhost/v1/"+command, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", response.Status, string(data))
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
