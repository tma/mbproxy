// Package health provides an HTTP health check server.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Checker reports whether a component is healthy.
type Checker interface {
	Healthy() error
}

// Response is the JSON body returned by the health endpoint.
type Response struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Server is a lightweight HTTP server that exposes a /health endpoint.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// NewServer creates a new health check server.
// The checker is called on each request to determine upstream health.
func NewServer(addr string, checker Checker, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	s := &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger: logger,
	}

	mux.HandleFunc("/health", s.handleHealth(checker))

	return s
}

func (s *Server) handleHealth(checker Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if err := checker.Healthy(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			resp := Response{Status: "unhealthy", Error: err.Error()}
			if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
				s.logger.Error("failed to encode health response", "error", encErr)
			}
			return
		}

		w.WriteHeader(http.StatusOK)
		resp := Response{Status: "ok"}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			s.logger.Error("failed to encode health response", "error", err)
		}
	}
}

// ListenAndServe starts the health server. It blocks until the server
// is shut down or encounters a fatal error. Use Listen + Serve to
// separate binding from serving, which allows detecting bind errors early.
func (s *Server) ListenAndServe() error {
	s.logger.Info("health server listening", "addr", s.httpServer.Addr)
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Listen binds the server to its configured address. Call Serve to
// start accepting connections after Listen returns successfully.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return nil, err
	}
	s.logger.Info("health server listening", "addr", ln.Addr())
	return ln, nil
}

// Serve accepts connections on the given listener. It blocks until the
// server is shut down or encounters a fatal error.
func (s *Server) Serve(ln net.Listener) error {
	err := s.httpServer.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully shuts down the health server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// CheckHealth performs an HTTP health check against the given address.
// It returns nil if the endpoint responds with 200 OK.
// Wildcard listen addresses (e.g. ":8080", "0.0.0.0:8080", "[::]:8080") are
// normalized to localhost so they can be used as dial targets. IPv6 addresses
// are handled correctly via net.JoinHostPort.
func CheckHealth(addr string) error {
	// Resolve the address so we can build a proper URL.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	// Normalize wildcard and empty hosts to localhost.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}

	url := fmt.Sprintf("http://%s/health", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}
