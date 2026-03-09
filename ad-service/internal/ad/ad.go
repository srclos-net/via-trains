package ad

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"via-trains/ad-service/internal/fault"
)

var tracer = otel.Tracer("ad-service/ad")

// ─── Ad catalog ──────────────────────────────────────────────────────────────

type Ad struct {
	ID        string `json:"id"`
	Advertiser string `json:"advertiser"`
	Headline  string `json:"headline"`
	ImageURL  string `json:"image_url"`
	ClickURL  string `json:"click_url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// Static catalog — realistic Canadian rail advertisers for the demo
var catalog = []Ad{
	{
		ID: "ad-001", Advertiser: "Via Rail Rewards",
		Headline:  "Earn points on every journey",
		ImageURL:  "https://cdn.trains.srclos.net/ads/via-rewards.jpg",
		ClickURL:  "https://www.viarail.ca/en/offers/via-preference",
		Width: 728, Height: 90,
	},
	{
		ID: "ad-002", Advertiser: "Fairmont Hotels",
		Headline:  "Stay in style at your destination",
		ImageURL:  "https://cdn.trains.srclos.net/ads/fairmont.jpg",
		ClickURL:  "https://www.fairmont.com",
		Width: 728, Height: 90,
	},
	{
		ID: "ad-003", Advertiser: "Porter Airlines",
		Headline:  "Where the train can't take you",
		ImageURL:  "https://cdn.trains.srclos.net/ads/porter.jpg",
		ClickURL:  "https://www.flyporter.com",
		Width: 728, Height: 90,
	},
	{
		ID: "ad-004", Advertiser: "Tourism Ottawa",
		Headline:  "Discover the capital this weekend",
		ImageURL:  "https://cdn.trains.srclos.net/ads/ottawa.jpg",
		ClickURL:  "https://ottawatourism.ca",
		Width: 728, Height: 90,
	},
	{
		ID: "ad-005", Advertiser: "CN Tower Experience",
		Headline:  "Toronto's icon awaits",
		ImageURL:  "https://cdn.trains.srclos.net/ads/cntower.jpg",
		ClickURL:  "https://www.cntower.ca",
		Width: 728, Height: 90,
	},
}

// ─── Impression store (in-memory for demo) ────────────────────────────────────

type Impression struct {
	AdID      string    `json:"ad_id"`
	KioskID   string    `json:"kiosk_id"`
	Station   string    `json:"station"`
	Timestamp time.Time `json:"timestamp"`
}

type Click struct {
	AdID      string    `json:"ad_id"`
	KioskID   string    `json:"kiosk_id"`
	Timestamp time.Time `json:"timestamp"`
}

type store struct {
	mu          sync.RWMutex
	impressions []Impression
	clicks      []Click
	impCount    atomic.Int64
	clickCount  atomic.Int64
}

var globalStore = &store{}

// ─── CDN simulation ───────────────────────────────────────────────────────────

// fetchFromCDN simulates an outbound HTTP call to a CDN for ad creative.
// This adds a downstream client span visible in Tempo.
func fetchFromCDN(ctx context.Context, ad *Ad) error {
	_, span := tracer.Start(ctx, "ad.cdn_fetch")
	defer span.End()
	span.SetAttributes(
		attribute.String("cdn.url", ad.ImageURL),
		attribute.String("ad.id", ad.ID),
		attribute.String("ad.advertiser", ad.Advertiser),
	)

	f := fault.Get()

	// slow_db fault reused as "slow CDN" for the demo story
	if f.IsActive(fault.ModeSlowCDN) {
		delay := time.Duration(rand.Intn(1500)+1000) * time.Millisecond
		span.SetAttributes(attribute.Bool("fault.slow_cdn", true))
		span.SetAttributes(attribute.Int64("fault.delay_ms", delay.Milliseconds()))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Simulate a fast CDN hit (no real network call needed for demo)
	simulatedLatency := time.Duration(rand.Intn(40)+10) * time.Millisecond
	time.Sleep(simulatedLatency)

	span.SetAttributes(attribute.Int64("cdn.latency_ms", simulatedLatency.Milliseconds()))
	return nil
}

// ─── Handler ──────────────────────────────────────────────────────────────────

type Handler struct {
	faults *fault.State
	client *http.Client
}

func NewHandler() *Handler {
	return &Handler{
		faults: fault.Get(),
		// Wrap outbound HTTP client with OTel so CDN calls produce client spans
		client: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   5 * time.Second,
		},
	}
}

func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/health", h.Health)

	r.Route("/api", func(r chi.Router) {
		r.Get("/ads",              h.GetAd)
		r.Post("/ads/impressions", h.TrackImpression)
		r.Post("/ads/clicks",      h.TrackClick)
		r.Get("/ads/stats",        h.GetStats)
	})
}

// GET /api/ads — returns one ad, simulates CDN fetch for the creative
func (h *Handler) GetAd(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.get_ad")
        _ = ctx
	defer span.End()

	// Fault: missing_ads → 503
	if h.faults.IsActive(fault.ModeMissingAds) {
		err := fmt.Errorf("ad CDN unreachable (fault injected)")
		span.RecordError(err)
		span.SetStatus(codes.Error, "missing_ads fault active")
		span.SetAttributes(attribute.Bool("fault.missing_ads", true))
		writeError(w, http.StatusServiceUnavailable, "Ad service unavailable")
		return
	}

	// Pick a random ad from catalog
	ad := catalog[rand.Intn(len(catalog))]
	span.SetAttributes(
		attribute.String("ad.id", ad.ID),
		attribute.String("ad.advertiser", ad.Advertiser),
	)

	// Simulate fetching the creative from CDN (adds a child span)
	if err := fetchFromCDN(ctx, &ad); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "cdn fetch failed")
		writeError(w, http.StatusGatewayTimeout, "CDN timeout")
		return
	}

	writeJSON(w, http.StatusOK, ad)
}

// POST /api/ads/impressions — record that an ad was displayed
func (h *Handler) TrackImpression(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.track_impression")
        _ = ctx
	defer span.End()

	var req struct {
		AdID    string `json:"ad_id"`
		KioskID string `json:"kiosk_id"`
		Station string `json:"station"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	span.SetAttributes(
		attribute.String("ad.id", req.AdID),
		attribute.String("kiosk.id", req.KioskID),
		attribute.String("kiosk.station", req.Station),
	)

	imp := Impression{
		AdID: req.AdID, KioskID: req.KioskID,
		Station: req.Station, Timestamp: time.Now().UTC(),
	}

	globalStore.mu.Lock()
	globalStore.impressions = append(globalStore.impressions, imp)
	globalStore.mu.Unlock()
	globalStore.impCount.Add(1)

	log.Debug().Str("ad_id", req.AdID).Str("station", req.Station).Msg("impression tracked")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// POST /api/ads/clicks — record that a user tapped an ad
func (h *Handler) TrackClick(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handler.track_click")
	defer span.End()

	var req struct {
		AdID    string `json:"ad_id"`
		KioskID string `json:"kiosk_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	span.SetAttributes(
		attribute.String("ad.id", req.AdID),
		attribute.String("kiosk.id", req.KioskID),
	)

	click := Click{AdID: req.AdID, KioskID: req.KioskID, Timestamp: time.Now().UTC()}

	globalStore.mu.Lock()
	globalStore.clicks = append(globalStore.clicks, click)
	globalStore.mu.Unlock()
	globalStore.clickCount.Add(1)

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "recorded"})
}

// GET /api/ads/stats — impression and click totals (shown on control panel)
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"impressions": globalStore.impCount.Load(),
		"clicks":      globalStore.clickCount.Load(),
	})
}

// GET /health
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "ad-service",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
