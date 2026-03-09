package booking

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"via-trains/booking-service/internal/db"
	"via-trains/booking-service/internal/fault"
)

var handlerTracer = otel.Tracer("booking-service/handler")

// Handler holds all HTTP handler dependencies.
type Handler struct {
	svc    *Service
	pool   *db.Pool
	faults *fault.State
}

func NewHandler(svc *Service, pool *db.Pool) *Handler {
	return &Handler{svc: svc, pool: pool, faults: fault.Get()}
}

// RegisterRoutes wires all routes onto the given router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/health", h.Health)

	r.Route("/api", func(r chi.Router) {
		// Booking endpoints
		r.Post("/bookings", h.CreateBooking)
		r.Get("/bookings/{ref}", h.GetBooking)
		r.Get("/availability", h.GetAvailability)

		// Fault injection control (called by the control panel UI)
		r.Post("/faults/{mode}", h.EnableFault)
		r.Delete("/faults/{mode}", h.DisableFault)
		r.Get("/faults", h.ListFaults)
	})
}

// ─── Health ──────────────────────────────────────────────────────────────────

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := h.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db ping failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":    "ok",
		"service":   "booking-service",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// ─── Bookings ────────────────────────────────────────────────────────────────

func (h *Handler) CreateBooking(w http.ResponseWriter, r *http.Request) {
	ctx, span := handlerTracer.Start(r.Context(), "handler.create_booking")
	defer span.End()

	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid request body")
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	span.SetAttributes(
		attribute.String("booking.origin", req.Origin),
		attribute.String("booking.destination", req.Destination),
		attribute.String("booking.class", string(req.SeatClass)),
	)

	booking, err := h.svc.Book(ctx, req)
	if err != nil {
		span.RecordError(err)
		if h.faults.IsActive(fault.ModeBookingFailure) {
			span.SetStatus(codes.Error, "fault: booking_failure")
			writeError(w, http.StatusServiceUnavailable, "Payment processor unavailable. Please try again.")
			return
		}
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Info().
		Str("reference", booking.Reference).
		Str("route", booking.Origin+" → "+booking.Destination).
		Str("class", string(booking.SeatClass)).
		Msg("booking confirmed")

	writeJSON(w, http.StatusCreated, booking)
}

func (h *Handler) GetBooking(w http.ResponseWriter, r *http.Request) {
	ctx, span := handlerTracer.Start(r.Context(), "handler.get_booking")
	defer span.End()

	ref := chi.URLParam(r, "ref")
	ref = strings.ToUpper(strings.TrimSpace(ref))
	span.SetAttributes(attribute.String("booking.reference", ref))

	booking, err := h.svc.Lookup(ctx, ref)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "not found")
		writeError(w, http.StatusNotFound, "booking not found: "+ref)
		return
	}

	writeJSON(w, http.StatusOK, booking)
}

// ─── Availability ─────────────────────────────────────────────────────────────

func (h *Handler) GetAvailability(w http.ResponseWriter, r *http.Request) {
	ctx, span := handlerTracer.Start(r.Context(), "handler.get_availability")
	defer span.End()

	q := r.URL.Query()
	req := AvailabilityRequest{
		Origin:      q.Get("origin"),
		Destination: q.Get("destination"),
		Date:        q.Get("date"),
	}

	span.SetAttributes(
		attribute.String("route.origin", req.Origin),
		attribute.String("route.destination", req.Destination),
		attribute.String("route.date", req.Date),
	)

	resp, err := h.svc.Availability(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── Fault control ───────────────────────────────────────────────────────────

func (h *Handler) EnableFault(w http.ResponseWriter, r *http.Request) {
	mode := fault.Mode(chi.URLParam(r, "mode"))
	if !validMode(mode) {
		writeError(w, http.StatusBadRequest, "unknown fault mode: "+string(mode))
		return
	}
	h.faults.Enable(mode)
	log.Warn().Str("mode", string(mode)).Msg("fault injected")
	writeJSON(w, http.StatusOK, faultStatus(h.faults))
}

func (h *Handler) DisableFault(w http.ResponseWriter, r *http.Request) {
	mode := fault.Mode(chi.URLParam(r, "mode"))
	h.faults.Disable(mode)
	log.Info().Str("mode", string(mode)).Msg("fault cleared")
	writeJSON(w, http.StatusOK, faultStatus(h.faults))
}

func (h *Handler) ListFaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, faultStatus(h.faults))
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func faultStatus(f *fault.State) map[string]any {
	active := f.ActiveModes()
	modes := make([]string, len(active))
	for i, m := range active {
		modes[i] = string(m)
	}
	return map[string]any{
		"active_faults":  modes,
		"slow_db_ms":     f.SlowDBDelay().Milliseconds(),
	}
}

func validMode(m fault.Mode) bool {
	switch m {
	case fault.ModeSlowDB, fault.ModeBookingFailure, fault.ModePoolExhaustion:
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error().Err(err).Msg("failed to encode response")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
