# Build stage
FROM golang:1.23-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates git

# Copy Go source before resolving modules so `go mod tidy` can calculate
# package-level checksums (go.mod alone is not enough for this project).
COPY go.mod ./
COPY cmd ./cmd
RUN go mod tidy && go mod verify

COPY web ./web
COPY test-results ./test-results
RUN CGO_ENABLED=0 GOOS=linux go build -mod=readonly -trimpath -ldflags="-s -w" -o /app/ipchat ./cmd/server

# Runtime stage
FROM alpine:3.21
WORKDIR /app
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /app/ipchat ./ipchat
COPY --from=builder /app/web ./web
COPY --from=builder /app/test-results ./test-results
EXPOSE 8080
USER 10001:10001
CMD ["./ipchat"]
