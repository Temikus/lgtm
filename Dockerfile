# syntax=docker/dockerfile:1
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /lgtm ./cmd/lgtm

# Docker actions must run as root to write the runner's output files, so
# this is distroless static, not static:nonroot.
FROM gcr.io/distroless/static-debian12
COPY --from=build /lgtm /lgtm
ENTRYPOINT ["/lgtm"]
