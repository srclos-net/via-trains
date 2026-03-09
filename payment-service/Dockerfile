FROM golang:1.25.4-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /payment-service ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata postgresql-client
WORKDIR /app
COPY --from=builder /payment-service .
COPY migrations/ ./migrations/
RUN addgroup -S app && adduser -S app -G app
USER app
EXPOSE 8082
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8082/health || exit 1
ENTRYPOINT ["/app/payment-service"]
