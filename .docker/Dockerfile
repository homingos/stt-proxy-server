ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-alpine AS builder

RUN apk add --no-cache git

RUN mkdir -p /app
WORKDIR /app


COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o ./server ./main.go

FROM alpine:latest

RUN apk add --no-cache ca-certificates

RUN mkdir -p /app
WORKDIR /app
COPY --from=builder /app/server .

EXPOSE 8027
ENV PORT=8027
CMD ["/app/server"]