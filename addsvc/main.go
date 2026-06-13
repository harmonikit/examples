// addsvc is the canonical harmonikit example — an "add" service exposed
// over HTTP, demonstrating endpoints, middleware, and transport binding.
//
// Run with:
//
//	go run . --addr=:8080
//
// Then:
//
//	curl -d '{"a":21,"b":21}' http://localhost:8080/add
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/harmonikit/harmoni/endpoint"
	"github.com/harmonikit/harmoni/log"
	"github.com/harmonikit/harmoni/middleware"
	httptransport "github.com/harmonikit/kit/transport/http"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	// ── Logger ────────────────────────────────────────────────────
	logger := log.NewSlogLogger(slog.Default())

	// ── Service ───────────────────────────────────────────────────
	svc := &addService{}
	logger.Log(log.LevelInfo, "service initialized", "name", svc.Name())

	// ── Endpoint ──────────────────────────────────────────────────
	addEP := endpoint.Endpoint[addRequest, addResponse](svc.Add)

	// ── Middleware ────────────────────────────────────────────────
	addEP = endpoint.Chain(
		middleware.Timeout[addRequest, addResponse](5*time.Second),
		middleware.Recovery[addRequest, addResponse](),
	)(addEP)

	// ── HTTP Transport ────────────────────────────────────────────
	decode := func(ctx context.Context, r *http.Request) (addRequest, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return addRequest{}, err
		}
		var req addRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return addRequest{}, fmt.Errorf("invalid JSON: %w", err)
		}
		return req, nil
	}

	encode := func(_ context.Context, w http.ResponseWriter, resp addResponse) error {
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(resp)
	}

	server := httptransport.NewServer(addEP, decode, encode)

	// ── HTTP Server ───────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.Handle("/add", server)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}

	// ── Start ─────────────────────────────────────────────────────
	go func() {
		fmt.Printf("addsvc listening on %s\n", *addr)
		fmt.Printf("  curl -d '{\"a\":21,\"b\":21}' http://localhost%s/add\n", *addr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("\nshutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

// ── Domain types ─────────────────────────────────────────────────────

type addRequest struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addResponse struct {
	Sum   int    `json:"sum"`
	Error string `json:"error,omitempty"`
}

// ── Service ──────────────────────────────────────────────────────────

type addService struct{}

func (s *addService) Name() string { return "addsvc" }

func (s *addService) Add(_ context.Context, req addRequest) (addResponse, error) {
	if req.A < 0 || req.B < 0 {
		return addResponse{Error: "negative numbers not allowed"}, nil
	}
	return addResponse{Sum: req.A + req.B}, nil
}
