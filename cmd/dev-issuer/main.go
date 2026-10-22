// Command dev-issuer is a development-only token issuer (port 8515) for the analyst console.
// It mints short-lived EdDSA JWTs for three synthetic personas and publishes its JWKS. Its key is
// generated at startup and never leaves the process. Not for production use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// Persona is a synthetic console user.
type Persona struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Personas for local development.
var Personas = []Persona{
	{ID: "analyst-1", Name: "Analyst One", Role: auth.RoleAnalyst},
	{ID: "risk-1", Name: "Risk Engineer One", Role: auth.RoleRiskEngineer},
	{ID: "ml-1", Name: "ML Engineer One", Role: auth.RoleMLEngineer},
}

// TokenTTL bounds console sessions.
const TokenTTL = 8 * time.Hour

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(telemetry.Healthcheck(envOr("FRAUD_HEALTHCHECK_URL", "http://127.0.0.1:8515/healthz")))
	}
	log := telemetry.NewLogger("dev-issuer")
	if !auth.DevMode() { // it mints console tokens for anyone who asks (security audit)
		log.Error("dev-issuer runs only with FRAUD_ENV=dev")
		os.Exit(2)
	}
	signer, err := auth.NewSigner(envOr("FRAUD_ISSUER", "http://dev-issuer:8515"))
	if err != nil {
		log.Error("key generation failed", "err", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: envOr("FRAUD_ISSUER_ADDR", "127.0.0.1:8515"), Handler: newMux(signer), ReadHeaderTimeout: 2 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("dev-issuer listening", "addr", srv.Addr, "issuer", signer.Issuer, "kid", signer.Kid)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newMux(signer *auth.Signer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, signer.JWKS()) })
	mux.HandleFunc("GET /personas", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"personas": Personas})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Persona string `json:"persona"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"persona\": \"<id>\"}"})
			return
		}
		for _, p := range Personas {
			if p.ID == req.Persona {
				tok, err := signer.Mint(p.ID, p.Role, p.Name, TokenTTL)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mint failed"})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": int(TokenTTL.Seconds()), "persona": p})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown persona"})
	})
	return mux
}
