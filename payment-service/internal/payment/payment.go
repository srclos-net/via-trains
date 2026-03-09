package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"via-trains/payment-service/internal/fault"
)

var tracer = otel.Tracer("payment-service/payment")

// ─── Domain types ─────────────────────────────────────────────────────────────

type CardBrand string

const (
	BrandVisa       CardBrand = "visa"
	BrandMastercard CardBrand = "mastercard"
	BrandAmex       CardBrand = "amex"
)

type Transaction struct {
	ID            string    `json:"id"`
	BookingRef    string    `json:"booking_ref"`
	AmountCAD     float64   `json:"amount_cad"`
	CardBrand     CardBrand `json:"card_brand"`
	CardLast4     string    `json:"card_last4"`
	ProcessorRef  string    `json:"processor_ref"`   // the "Stripe" transaction ID
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

type ValidateRequest struct {
	CardNumber string `json:"card_number"` // 16-digit, we only inspect last 4
	ExpiryMM   int    `json:"expiry_mm"`
	ExpiryYY   int    `json:"expiry_yy"`
	CVV        string `json:"cvv"`
}

type ValidateResponse struct {
	Valid     bool      `json:"valid"`
	CardBrand CardBrand `json:"card_brand"`
	Last4     string    `json:"last4"`
}

type ChargeRequest struct {
	BookingRef string  `json:"booking_ref"`
	AmountCAD  float64 `json:"amount_cad"`
	CardNumber string  `json:"card_number"`
	ExpiryMM   int     `json:"expiry_mm"`
	ExpiryYY   int     `json:"expiry_yy"`
	CVV        string  `json:"cvv"`
}

// ─── Handler ──────────────────────────────────────────────────────────────────

type Handler struct {
	pool   *pgxpool.Pool
	faults *fault.State
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, faults: fault.Get()}
}

func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/health", h.Health)
	r.Route("/api", func(r chi.Router) {
		r.Post("/payments/validate", h.ValidateCard)
		r.Post("/payments/charge",   h.Charge)
		r.Get("/payments/{id}",      h.GetTransaction)
	})
}

// POST /api/payments/validate — card validation, no charge
func (h *Handler) ValidateCard(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.validate_card")
	defer span.End()

	var req ValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	resp, err := validateCard(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	span.SetAttributes(
		attribute.String("card.brand", string(resp.CardBrand)),
		attribute.String("card.last4", resp.Last4),
		attribute.Bool("card.valid", resp.Valid),
	)
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/payments/charge — validate + call "Stripe" + persist transaction
func (h *Handler) Charge(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.charge")
	defer span.End()

	var req ChargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	span.SetAttributes(
		attribute.String("booking.ref", req.BookingRef),
		attribute.Float64("payment.amount_cad", req.AmountCAD),
	)

	// Fault: payment_failure → 503 before we even try
	if h.faults.IsActive(fault.ModePaymentFailure) {
		err := fmt.Errorf("payment processor unavailable (fault injected)")
		span.RecordError(err)
		span.SetStatus(codes.Error, "payment_failure fault active")
		span.SetAttributes(attribute.Bool("fault.payment_failure", true))
		writeError(w, http.StatusServiceUnavailable, "Payment processor unavailable. Please try again.")
		return
	}

	// Step 1: validate card
	cardReq := ValidateRequest{
		CardNumber: req.CardNumber, ExpiryMM: req.ExpiryMM,
		ExpiryYY: req.ExpiryYY, CVV: req.CVV,
	}
	card, err := validateCard(ctx, cardReq)
	if err != nil || !card.Valid {
		span.SetStatus(codes.Error, "card validation failed")
		writeError(w, http.StatusUnprocessableEntity, "card validation failed")
		return
	}

	// Step 2: call external processor (Stripe-like) — this is the key downstream span
	processorRef, err := h.callProcessor(ctx, req, card)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "processor error")
		writeError(w, http.StatusBadGateway, "payment processor error: "+err.Error())
		return
	}

	// Step 3: persist transaction
	tx, err := h.persistTransaction(ctx, req, card, processorRef)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "persist failed")
		writeError(w, http.StatusInternalServerError, "failed to record transaction")
		return
	}

	span.SetAttributes(
		attribute.String("payment.transaction_id", tx.ID),
		attribute.String("payment.processor_ref", tx.ProcessorRef),
		attribute.String("payment.status", tx.Status),
	)

	log.Info().
		Str("transaction_id", tx.ID).
		Str("booking_ref", tx.BookingRef).
		Float64("amount", tx.AmountCAD).
		Str("card_brand", string(tx.CardBrand)).
		Msg("payment processed")

	writeJSON(w, http.StatusCreated, tx)
}

// GET /api/payments/:id
func (h *Handler) GetTransaction(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handler.get_transaction")
	defer span.End()

	id := chi.URLParam(r, "id")
	span.SetAttributes(attribute.String("payment.transaction_id", id))

	row := h.pool.QueryRow(ctx,
		`SELECT id, booking_ref, amount_cad, card_brand, card_last4, processor_ref, status, created_at
		 FROM transactions WHERE id = $1`, id)

	var tx Transaction
	if err := row.Scan(&tx.ID, &tx.BookingRef, &tx.AmountCAD, &tx.CardBrand,
		&tx.CardLast4, &tx.ProcessorRef, &tx.Status, &tx.CreatedAt); err != nil {
		span.SetStatus(codes.Error, "not found")
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	writeJSON(w, http.StatusOK, tx)
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db ping failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "payment-service"})
}

// ─── Internal ─────────────────────────────────────────────────────────────────

// callProcessor simulates an outbound call to a payment processor (Stripe et al).
// Produces a client span so Tempo shows the full chain:
//   handler.charge → payment.processor_call → (simulated network)
func (h *Handler) callProcessor(ctx context.Context, req ChargeRequest, card *ValidateResponse) (string, error) {
	ctx, span := tracer.Start(ctx, "payment.processor_call")
	defer span.End()

	span.SetAttributes(
		attribute.String("processor.name", "stripe-simulator"),
		attribute.String("processor.endpoint", "https://api.stripe.com/v1/charges"),
		attribute.Float64("processor.amount", req.AmountCAD),
		attribute.String("processor.currency", "cad"),
		attribute.String("card.brand", string(card.CardBrand)),
	)

	// Slow processor fault
	if h.faults.IsActive(fault.ModeSlowProcessor) {
		delay := time.Duration(rand.Intn(2000)+1500) * time.Millisecond
		span.SetAttributes(
			attribute.Bool("fault.slow_processor", true),
			attribute.Int64("fault.delay_ms", delay.Milliseconds()),
		)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		// Normal: 80–250ms network latency to processor
		time.Sleep(time.Duration(rand.Intn(170)+80) * time.Millisecond)
	}

	ref := "ch_" + strings.ToLower(uuid.New().String()[:16])
	span.SetAttributes(attribute.String("processor.ref", ref))
	return ref, nil
}

func (h *Handler) persistTransaction(ctx context.Context, req ChargeRequest, card *ValidateResponse, processorRef string) (*Transaction, error) {
	ctx, span := tracer.Start(ctx, "payment.persist_transaction")
	defer span.End()

	id := uuid.New().String()
	now := time.Now().UTC()

	_, err := h.pool.Exec(ctx,
		`INSERT INTO transactions (id, booking_ref, amount_cad, card_brand, card_last4, processor_ref, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'success', $7)`,
		id, req.BookingRef, req.AmountCAD, string(card.CardBrand), card.Last4, processorRef, now,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return &Transaction{
		ID: id, BookingRef: req.BookingRef, AmountCAD: req.AmountCAD,
		CardBrand: card.CardBrand, CardLast4: card.Last4,
		ProcessorRef: processorRef, Status: "success", CreatedAt: now,
	}, nil
}

// validateCard is pure logic — detects brand, checks expiry, produces a span
func validateCard(ctx context.Context, req ValidateRequest) (*ValidateResponse, error) {
	_, span := tracer.Start(ctx, "payment.validate_card")
	defer span.End()

	digits := strings.ReplaceAll(req.CardNumber, " ", "")
	if len(digits) < 13 {
		return nil, fmt.Errorf("invalid card number length")
	}

	now := time.Now()
	expYear := 2000 + req.ExpiryYY
	if expYear < now.Year() || (expYear == now.Year() && req.ExpiryMM < int(now.Month())) {
		return nil, fmt.Errorf("card expired")
	}

	brand := detectBrand(digits)
	last4 := digits[len(digits)-4:]

	span.SetAttributes(
		attribute.String("card.brand", string(brand)),
		attribute.String("card.last4", last4),
	)

	return &ValidateResponse{Valid: true, CardBrand: brand, Last4: last4}, nil
}

func detectBrand(digits string) CardBrand {
	switch {
	case strings.HasPrefix(digits, "4"):
		return BrandVisa
	case strings.HasPrefix(digits, "5") || strings.HasPrefix(digits, "2"):
		return BrandMastercard
	case strings.HasPrefix(digits, "34") || strings.HasPrefix(digits, "37"):
		return BrandAmex
	default:
		return BrandVisa // default for demo
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
