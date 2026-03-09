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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"via-trains/payment-service/internal/fault"
	"via-trains/payment-service/internal/middleware"
	"via-trains/payment-service/internal/payment"
	"via-trains/payment-service/internal/tracing"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	if os.Getenv("LOG_PRETTY") == "true" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	log.Info().Msg("starting payment-service")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// OTel
	shutdown, err := tracing.Init(ctx, "payment-service", "1.0.0")
	if err != nil {
		log.Warn().Err(err).Msg("tracing init failed")
	} else {
		defer shutdown(ctx)
	}

	// Fault poller
	bookingURL := getEnv("BOOKING_SERVICE_URL", "http://booking-service")
	fault.StartPoller(ctx, bookingURL, 5*time.Second)

	// Database
	dsn := mustEnv("DATABASE_URL")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("db connect failed")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatal().Err(err).Msg("db ping failed")
	}
	log.Info().Msg("database connected")

	// Router
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.CORS)
	r.Use(middleware.Logger)

	h := payment.NewHandler(pool)
	h.RegisterRoutes(r)

	port := getEnv("PORT", "8082")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      otelhttp.NewHandler(r, "payment-service"),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second,
	}

	go func() {
		log.Info().Str("port", port).Msg("listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down payment-service...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
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
