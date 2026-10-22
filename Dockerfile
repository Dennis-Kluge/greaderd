# syntax=docker/dockerfile:1

# Build a static binary; the SQLite driver is pure Go, so cgo is not needed.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/greaderd ./cmd/greaderd \
 && mkdir -p /out/data

# distroless has no shell; the binary checks its own health.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/greaderd /greaderd
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV GREADERD_ADDR=:8080 \
    GREADERD_DB=/data/greaderd.db
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/greaderd", "healthcheck"]
ENTRYPOINT ["/greaderd"]
