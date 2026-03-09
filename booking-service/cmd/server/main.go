package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"via-trains/booking-service/internal/booking"
	"via-trains/booking-service/internal/db"
	"via-trains/booking-service/internal/middleware"
	"via-trains/booking-service/internal/tracing"
)

func main() {
	// ── Logging ───────────────────────────────────────────────────────────────
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	if os.Getenv("LOG_PRETTY") == "true" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	log.Info().Msg("starting booking-service")

	// ── OTel tracing ──────────────────────────────────────────────────────────
	ctx := context.Background()
	shutdown, err := tracing.Init(ctx, "booking-service", "1.0.0")
	if err != nil {
		// Non-fatal: if OTLP endpoint isn't set, traces just won't export
		log.Warn().Err(err).Msg("tracing init failed — running without traces")
	} else {
		defer func() {
			if err := shutdown(ctx); err != nil {
				log.Error().Err(err).Msg("tracer shutdown error")
			}
		}()
	}

	// ── Database ──────────────────────────────────────────────────────────────
	dsn := mustEnv("DATABASE_URL")
	pool, err := db.New(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer pool.Close()
	log.Info().Msg("database connected")

	// ── Domain ────────────────────────────────────────────────────────────────
	repo := booking.NewRepository(pool)
	svc  := booking.NewService(repo)
	h    := booking.NewHandler(svc, pool)

	// ── Router ────────────────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.CORS)
	r.Use(middleware.Logger)

	// Wrap the entire mux with OTel HTTP instrumentation — every request gets
	// a server span with http.method, http.route, http.status_code attributes
	h.RegisterRoutes(r)

	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      otelhttp.NewHandler(r, "booking-service"),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info().Str("port", port).Msg("listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	<-stop
	log.Info().Msg("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("server shutdown error")
	}
	log.Info().Msg("stopped")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatal().Str("key", key).Msg("required env var not set")
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
