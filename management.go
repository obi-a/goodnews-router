package goodnews

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (r *Runtime) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, request *http.Request, value any, status int) {
		w.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/v1/interfaces", func(w http.ResponseWriter, req *http.Request) {
		write(w, req, struct {
			Interfaces  []Interface     `json:"interfaces"`
			Protections map[string]bool `json:"effective_protections"`
		}{r.Engine.Config.Interfaces, map[string]bool{
			"verify_ipv4_checksum": enabled(r.Engine.Config.Protections.VerifyIPv4Checksum),
			"decrement_ttl":        enabled(r.Engine.Config.Protections.DecrementTTL),
		}}, http.StatusOK)
	})
	mux.HandleFunc("/v1/routes", func(w http.ResponseWriter, req *http.Request) {
		t := r.Engine.FIB.Load()
		write(w, req, struct {
			Version uint64  `json:"version"`
			Routes  []Route `json:"routes"`
		}{t.Version(), t.Routes()}, http.StatusOK)
	})
	mux.HandleFunc("/v1/counters", func(w http.ResponseWriter, req *http.Request) { write(w, req, r.Snapshot(), http.StatusOK) })
	for _, subsystem := range []string{"arp", "neighbors", "lsdb"} {
		mux.HandleFunc("/v1/"+subsystem, func(w http.ResponseWriter, req *http.Request) {
			write(w, req, map[string]any{"enabled": false, "subsystem": subsystem, "message": "not enabled in Stage 1"}, http.StatusNotImplemented)
		})
	}
	return mux
}

func ServeManagement(ctx context.Context, r *Runtime, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	// Never unlink an existing socket: it might belong to a running router.
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("management socket (already in use or stale?): %w", err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: r.ManagementHandler(), ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
