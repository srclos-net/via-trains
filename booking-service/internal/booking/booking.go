package booking

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"via-trains/booking-service/internal/db"
	"via-trains/booking-service/internal/fault"
)

var tracer = otel.Tracer("booking-service/booking")

// ─── Domain types ─────────────────────────────────────────────────────────────

type Class string

const (
	ClassEconomy  Class = "economy"
	ClassBusiness Class = "business"
)

type Status string

const (
	StatusConfirmed  Status = "confirmed"
	StatusCancelled  Status = "cancelled"
)

type Booking struct {
	ID          int64     `json:"id"`
	Reference   string    `json:"reference"`    // e.g. "VIA-A1B2C3"
	Origin      string    `json:"origin"`
	Destination string    `json:"destination"`
	DepartureAt time.Time `json:"departure_at"`
	SeatClass   Class     `json:"seat_class"`
	Passenger   string    `json:"passenger"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type CreateRequest struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	DepartureAt string `json:"departure_at"` // RFC3339
	SeatClass   Class  `json:"seat_class"`
	Passenger   string `json:"passenger"`
}

type AvailabilityRequest struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Date        string `json:"date"` // YYYY-MM-DD
}

type AvailabilityResponse struct {
	Origin      string     `json:"origin"`
	Destination string     `json:"destination"`
	Trains      []TrainOpt `json:"trains"`
}

type TrainOpt struct {
	TrainID     string    `json:"train_id"`
	DepartureAt time.Time `json:"departure_at"`
	Economy     SeatInfo  `json:"economy"`
	Business    SeatInfo  `json:"business"`
}

type SeatInfo struct {
	Available int     `json:"available"`
	PriceCAD  float64 `json:"price_cad"`
}

// ─── Repository ───────────────────────────────────────────────────────────────

type Repository struct {
	pool   *db.Pool
	faults *fault.State
}

func NewRepository(pool *db.Pool) *Repository {
	return &Repository{pool: pool, faults: fault.Get()}
}

func (r *Repository) Create(ctx context.Context, req CreateRequest) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "booking.repository.create")
	defer span.End()

	span.SetAttributes(
		attribute.String("booking.origin", req.Origin),
		attribute.String("booking.destination", req.Destination),
		attribute.String("booking.class", string(req.SeatClass)),
		attribute.String("booking.passenger", req.Passenger),
	)

	dep, err := time.Parse(time.RFC3339, req.DepartureAt)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "invalid departure_at")
		return nil, fmt.Errorf("invalid departure_at: %w", err)
	}

	ref := generateReference()
	span.SetAttributes(attribute.String("booking.reference", ref))

	err = r.pool.Exec(ctx,
		`INSERT INTO bookings (reference, origin, destination, departure_at, seat_class, passenger, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'confirmed')`,
		ref, req.Origin, req.Destination, dep, string(req.SeatClass), req.Passenger,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "db insert failed")
		return nil, fmt.Errorf("insert booking: %w", err)
	}

	// Read it back so we get the DB-generated id and created_at
	return r.FindByRef(ctx, ref)
}

func (r *Repository) FindByRef(ctx context.Context, ref string) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "booking.repository.find_by_ref")
	defer span.End()
	span.SetAttributes(attribute.String("booking.reference", ref))

	row, release := r.pool.QueryRow(ctx,
		`SELECT id, reference, origin, destination, departure_at, seat_class, passenger, status, created_at
		 FROM bookings WHERE reference = $1`,
		ref,
	)
	defer release()

	var b Booking
	err := row.Scan(
		&b.ID, &b.Reference, &b.Origin, &b.Destination,
		&b.DepartureAt, &b.SeatClass, &b.Passenger, &b.Status, &b.CreatedAt,
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "not found")
		return nil, fmt.Errorf("find booking %s: %w", ref, err)
	}

	return &b, nil
}

// ─── Service ─────────────────────────────────────────────────────────────────

// Service adds business logic on top of the repository.
type Service struct {
	repo   *Repository
	faults *fault.State
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo, faults: fault.Get()}
}

func (s *Service) Book(ctx context.Context, req CreateRequest) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "booking.service.book")
	defer span.End()

	// Fault: booking_failure — surface as a hard error with span annotation
	if s.faults.IsActive(fault.ModeBookingFailure) {
		err := fmt.Errorf("payment processor unavailable (fault injected)")
		span.RecordError(err)
		span.SetStatus(codes.Error, "booking_failure fault active")
		span.SetAttributes(attribute.Bool("fault.booking_failure", true))
		return nil, err
	}

	// Basic validation
	req.Origin      = strings.TrimSpace(req.Origin)
	req.Destination = strings.TrimSpace(req.Destination)
	req.Passenger   = strings.TrimSpace(req.Passenger)

	if req.Origin == "" || req.Destination == "" {
		return nil, fmt.Errorf("origin and destination are required")
	}
	if req.Origin == req.Destination {
		return nil, fmt.Errorf("origin and destination must differ")
	}
	if req.SeatClass != ClassEconomy && req.SeatClass != ClassBusiness {
		return nil, fmt.Errorf("seat_class must be 'economy' or 'business'")
	}
	if req.Passenger == "" {
		req.Passenger = "Guest"
	}

	b, err := s.repo.Create(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	span.SetAttributes(
		attribute.String("booking.reference", b.Reference),
		attribute.String("booking.status", string(b.Status)),
	)
	return b, nil
}

func (s *Service) Lookup(ctx context.Context, ref string) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "booking.service.lookup")
	defer span.End()
	span.SetAttributes(attribute.String("booking.reference", ref))

	b, err := s.repo.FindByRef(ctx, ref)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return b, nil
}

// Availability returns simulated seat availability for demo purposes.
// In a real system this would query the bookings table for taken seats.
func (s *Service) Availability(ctx context.Context, req AvailabilityRequest) (*AvailabilityResponse, error) {
	ctx, span := tracer.Start(ctx, "booking.service.availability")
	defer span.End()
	span.SetAttributes(
		attribute.String("route.origin", req.Origin),
		attribute.String("route.destination", req.Destination),
		attribute.String("route.date", req.Date),
	)

	// Simulate a DB read (subject to slow_db fault via pool)
	_, release := s.repo.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM bookings WHERE origin=$1 AND destination=$2 AND departure_at::date=$3::date`,
		req.Origin, req.Destination, req.Date,
	)
	release()

	// Build a few realistic-looking VIA Rail trains
	date, _ := time.Parse("2006-01-02", req.Date)
	if date.IsZero() {
		date = time.Now().Truncate(24 * time.Hour)
	}

	trains := []TrainOpt{
		makeTrainOpt("VIA-60",  date, 7, 30),
		makeTrainOpt("VIA-62",  date, 10, 45),
		makeTrainOpt("VIA-64",  date, 14, 15),
		makeTrainOpt("VIA-640", date, 17, 0),
	}

	return &AvailabilityResponse{
		Origin:      req.Origin,
		Destination: req.Destination,
		Trains:      trains,
	}, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeTrainOpt(id string, date time.Time, hour, minute int) TrainOpt {
	dep := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, time.UTC)
	return TrainOpt{
		TrainID:     id,
		DepartureAt: dep,
		Economy: SeatInfo{
			Available: rand.Intn(40) + 5,
			PriceCAD:  float64(rand.Intn(80)+60),
		},
		Business: SeatInfo{
			Available: rand.Intn(10) + 1,
			PriceCAD:  float64(rand.Intn(120)+180),
		},
	}
}

func generateReference() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return "VIA-" + string(b)
}

// ensure uuid import used
var _ = uuid.New
