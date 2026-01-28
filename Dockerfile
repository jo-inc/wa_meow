FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/server/main.go ./cmd/server/

RUN CGO_ENABLED=1 GOOS=linux go build -o whatsapp-server ./cmd/server

FROM alpine:3.19

RUN apk add --no-cache sqlite-libs ca-certificates

WORKDIR /app

COPY --from=builder /app/whatsapp-server .

RUN mkdir -p /data/whatsapp

ENV DATA_DIR=/data/whatsapp
ENV PORT=8090

EXPOSE 8090

CMD ["./whatsapp-server"]
