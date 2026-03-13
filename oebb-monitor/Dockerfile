FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /oebb-monitor .

FROM alpine:latest
RUN apk add --no-cache tzdata ca-certificates
COPY --from=builder /oebb-monitor /oebb-monitor
ENV TZ=Europe/Vienna
EXPOSE 80
ENTRYPOINT ["/oebb-monitor"]
