# booking-service

Go REST API backing the VIA Rail kiosk demo. Provides a realistic booking flow with full OpenTelemetry tracing from HTTP handler → service layer → Postgres, all visible in Grafana Cloud Tempo.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness + DB ping |
| `POST` | `/api/bookings` | Create a booking |
| `GET` | `/api/bookings/:ref` | Look up by reference (e.g. `VIA-A1B2C3`) |
| `GET` | `/api/availability?origin=Toronto&destination=Montreal&date=2026-03-06` | Seat availability |
| `POST` | `/api/faults/:mode` | Enable a fault |
| `DELETE` | `/api/faults/:mode` | Disable a fault |
| `GET` | `/api/faults` | List active faults |

## Fault modes

| Mode | What it does |
|------|-------------|
| `slow_db` | Adds a 2s sleep before every DB query — shows up as long DB spans in Tempo |
| `booking_failure` | Returns 503 from POST /bookings — payment processor simulated down |
| `pool_exhaustion` | Simulates connection pool exhaustion — DB acquire times out |

## Trace story

Every request produces a span tree like:

```
POST /api/bookings                        [otelhttp — http.server]
  └─ handler.create_booking               [booking/handler]
       └─ booking.service.book            [booking/service]
            └─ booking.repository.create  [booking/repository]
                 ├─ db.exec               [db layer — slow_db fault visible here]
                 │    └─ pgx.query        [otelpgx — actual SQL]
                 └─ booking.repository.find_by_ref
                      └─ db.query_row
                           └─ pgx.query
```

## Local dev

```bash
docker compose up
curl -X POST http://localhost:8080/api/bookings \
  -H 'Content-Type: application/json' \
  -d '{"origin":"Toronto","destination":"Montreal","departure_at":"2026-03-06T09:00:00Z","seat_class":"economy","passenger":"Jane Smith"}'
```

## Grafana Cloud Tempo

Set these env vars (from your stack's "Send data → OpenTelemetry" page):

```
OTEL_EXPORTER_OTLP_ENDPOINT=https://tempo-prod-XX-...grafana.net
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <base64(instanceID:apikey)>
```

## Deploy to k8s

```bash
# 1. Edit the secret values in k8s/manifests.yaml
# 2. Build and push
docker build -t harbor.srclos.net/trains/booking-service:latest .
docker push harbor.srclos.net/trains/booking-service:latest

# 3. Apply
kubectl apply -f k8s/manifests.yaml
```
