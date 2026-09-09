# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — build
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Cache dependency downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build the server binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /trailog ./cmd/trailog

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — runtime (distroless for minimal attack surface)
# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /trailog /trailog

EXPOSE 8080

ENTRYPOINT ["/trailog"]
