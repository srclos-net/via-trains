package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"via-trains/booking-service/internal/fault"
)

var tracer trace.Tracer

func init() {
	tracer = otel.Tracer("booking-service/db")
}

// Pool wraps pgxpool with fault injection awareness.
type Pool struct {
	inner  *pgxpool.Pool
	faults *fault.State
}

// New creates a traced pgx connection pool.
func New(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Pool sizing — small for demo, pool exhaustion fault is visible
	cfg.MaxConns = 5
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return &Pool{inner: pool, faults: fault.Get()}, nil
}

// Acquire gets a connection, with optional pool exhaustion fault simulation.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if p.faults.IsActive(fault.ModePoolExhaustion) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
	}
	return p.inner.Acquire(ctx)
}

// Close shuts down the pool.
func (p *Pool) Close() { p.inner.Close() }

// Ping checks connectivity.
func (p *Pool) Ping(ctx context.Context) error { return p.inner.Ping(ctx) }

// QueryRow executes a query with slow_db fault injection and OTel span.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) (pgx.Row, func()) {
	ctx, span := tracer.Start(ctx, "db.query_row")
	span.SetAttributes(attribute.String("db.statement", sql))

	p.maybeSlowDB(ctx, span)

	conn, err := p.inner.Acquire(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return errorRow{err: err}, func() {}
	}

	row := conn.QueryRow(ctx, sql, args...)
	return row, func() {
		conn.Release()
		span.End()
	}
}

// Exec executes a statement with slow_db fault injection and OTel span.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) error {
	ctx, span := tracer.Start(ctx, "db.exec")
	span.SetAttributes(attribute.String("db.statement", sql))
	defer span.End()

	p.maybeSlowDB(ctx, span)

	conn, err := p.inner.Acquire(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, sql, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (p *Pool) maybeSlowDB(ctx context.Context, span interface{ SetAttributes(...attribute.KeyValue) }) {
	if p.faults.IsActive(fault.ModeSlowDB) {
		delay := p.faults.SlowDBDelay()
		span.SetAttributes(
			attribute.Bool("fault.slow_db", true),
			attribute.Int64("fault.slow_db_ms", delay.Milliseconds()),
		)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
}

// errorRow is a pgx.Row that always returns an error (for pool exhaustion path).
type errorRow struct{ err error }

func (e errorRow) Scan(dest ...any) error { return e.err }
