# Multi-stage build shared by every service in the monorepo.
# Pick the binary with:  --build-arg SERVICE=bot|core|scraper|notifier
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
ARG SERVICE
WORKDIR /src

# Dependencies first so the layer is cached between code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN test -n "$SERVICE" || (echo "build-arg SERVICE is required" && exit 1) \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app
COPY --from=build /out/app /usr/local/bin/app
USER app
ENTRYPOINT ["/usr/local/bin/app"]
