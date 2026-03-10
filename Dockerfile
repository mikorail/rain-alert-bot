# ---- Build stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o rain-alert-bot .

# ---- Runtime stage ----
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

# Create non-root user
RUN addgroup -S botgroup && adduser -S botuser -G botgroup

WORKDIR /app

COPY --from=builder /app/rain-alert-bot .

# Create data directory with correct ownership
RUN mkdir -p /app/data && chown -R botuser:botgroup /app

ENV TZ=Asia/Jakarta

EXPOSE 8080

# Run as non-root
USER botuser

CMD ["./rain-alert-bot"]
