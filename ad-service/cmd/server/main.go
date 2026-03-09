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

	"via-trains/ad-service/internal/ad"
	"via-trains/ad-service/internal/fault"
	"via-trains/ad-service/internal/middleware"
	"via-trains/ad-service/internal/tracing"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	if os.Getenv("LOG_PRETTY") == "true" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	log.Info().Msg("starting ad-service")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// OTel tracing
	shutdown, err := tracing.Init(ctx, "ad-service", "1.0.0")
	if err != nil {
		log.Warn().Err(err).Msg("tracing init failed — running without traces")
	} else {
		defer shutdown(ctx)
	}

	// Start polling booking-service for fault state every 5s
	bookingURL := getEnv("BOOKING_SERVICE_URL", "http://booking-service")
	fault.StartPoller(ctx, bookingURL, 5*time.Second)
	log.Info().Str("booking_url", bookingURL).Msg("fault poller started")

	// Router
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.CORS)
	r.Use(middleware.Logger)

	h := ad.NewHandler()
	h.RegisterRoutes(r)

	port := getEnv("PORT", "8081")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      otelhttp.NewHandler(r, "ad-service"),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		log.Info().Str("port", port).Msg("listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutting down ad-service...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
