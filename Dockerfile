# ---- Build stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o rain-alert-bot .

# ---- Runtime stage ----
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/rain-alert-bot .

# Create a data directory for subscriber persistence
RUN mkdir -p /app/data

ENV TZ=Asia/Jakarta

EXPOSE 8080

CMD ["./rain-alert-bot"]
