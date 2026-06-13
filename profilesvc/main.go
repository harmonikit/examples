// profilesvc demonstrates a more complex harmonikit service with multiple
// endpoints, JWT authentication, rate limiting, and middleware composition.
//
// Run with:
//
//	go run . --addr=:8080
//
// Endpoints:
//
//	GET  /profile?id=1          — Get a profile
//	POST /profile                — Create a profile (requires JWT auth)
//
// Rate limit: 100 req/s, burst 10.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/harmonikit/harmoni/auth"
	"github.com/harmonikit/harmoni/endpoint"
	"github.com/harmonikit/harmoni/log"
	"github.com/harmonikit/harmoni/middleware"
	jwtauth "github.com/harmonikit/kit/auth/jwt"
	tb "github.com/harmonikit/kit/ratelimit/tokenbucket"
	httptransport "github.com/harmonikit/kit/transport/http"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	// ── Logger ────────────────────────────────────────────────────
	logger := log.NewSlogLogger(slog.Default())

	// ── Service ───────────────────────────────────────────────────
	svc := &profileService{profiles: make(map[int]profile)}
	logger.Log(log.LevelInfo, "service initialized", "name", svc.Name())

	// ── Rate limiter ──────────────────────────────────────────────
	limiter := tb.New(100, 10) // 100 req/s, burst 10

	// ── JWT Auth ──────────────────────────────────────────────────
	// Simple demo auth: validates that the token starts with "demo-".
	jwtAuth := jwtauth.New[profileRequest](extractToken, validateToken)

	// ── Endpoints ─────────────────────────────────────────────────
	getEP := endpoint.Endpoint[profileRequest, profileResponse](svc.Get)
	setEP := endpoint.Endpoint[profileRequest, profileResponse](svc.Set)

	// ── Middleware ────────────────────────────────────────────────
	baseMW := endpoint.Chain(
		middleware.Timeout[profileRequest, profileResponse](5*time.Second),
		middleware.Recovery[profileRequest, profileResponse](),
		tb.Middleware[profileRequest, profileResponse](limiter),
	)

	// Get does not require auth.
	getEP = baseMW(getEP)
	// Set requires JWT auth.
	setEP = endpoint.Chain(baseMW, auth.Middleware[profileRequest, profileResponse](jwtAuth))(setEP)

	// ── HTTP Transport ────────────────────────────────────────────
	decode := func(_ context.Context, r *http.Request) (profileRequest, error) {
		var req profileRequest
		switch r.Method {
		case http.MethodGet:
			id, _ := strconv.Atoi(r.URL.Query().Get("id"))
			req.ID = id
		case http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return req, err
			}
			if err := json.Unmarshal(body, &req); err != nil {
				return req, fmt.Errorf("invalid JSON: %w", err)
			}
		}
		return req, nil
	}

	encode := func(_ context.Context, w http.ResponseWriter, resp profileResponse) error {
		w.Header().Set("Content-Type", "application/json")
		if resp.Error != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
		return json.NewEncoder(w).Encode(resp)
	}

	getServer := httptransport.NewServer(getEP, decode, encode)
	setServer := httptransport.NewServer(setEP, decode, encode)

	// ── Routes ────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.Handle("/profile", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getServer.ServeHTTP(w, r)
		case http.MethodPost:
			setServer.ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{Addr: *addr, Handler: mux}

	// ── Start ─────────────────────────────────────────────────────
	go func() {
		fmt.Printf("profilesvc listening on %s\n", *addr)
		fmt.Printf("  curl http://localhost%s/profile?id=1\n", *addr)
		fmt.Printf("  curl -XPOST -d '{\"name\":\"Alice\",\"email\":\"alice@example.com\"}' -H 'Authorization: Bearer demo-token' http://localhost%s/profile\n", *addr)
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

type profile struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type profileRequest struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type profileResponse struct {
	Profile *profile `json:"profile,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ── Service ──────────────────────────────────────────────────────────

type profileService struct {
	mu       sync.Mutex
	profiles map[int]profile
	nextID   int
}

func (s *profileService) Name() string { return "profilesvc" }

func (s *profileService) Get(_ context.Context, req profileRequest) (profileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[req.ID]
	if !ok {
		return profileResponse{Error: "profile not found"}, nil
	}
	return profileResponse{Profile: &p}, nil
}

func (s *profileService) Set(_ context.Context, req profileRequest) (profileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	p := profile{
		ID:    s.nextID,
		Name:  req.Name,
		Email: req.Email,
	}
	s.profiles[p.ID] = p
	return profileResponse{Profile: &p}, nil
}

// ── JWT helpers ──────────────────────────────────────────────────────

func extractToken(_ context.Context, req profileRequest) (string, error) {
	// In production, the token comes from the HTTP Authorization header.
	// For the example, we embed it in the request (it would be extracted
	// by the HTTP decode function from headers).
	return req.Email, nil
}

func validateToken(_ context.Context, token string) (any, error) {
	if !strings.HasPrefix(token, "demo-") {
		return nil, fmt.Errorf("invalid token")
	}
	return map[string]any{"sub": "demo-user"}, nil
}
