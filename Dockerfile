# Build a static binary, then run it on a minimal image as a non-root user.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trader ./cmd/trader \
 && mkdir -p /out/data

# distroless/static has CA certificates for HTTPS; time zones are embedded in the binary.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/trader /app/trader
COPY config ./config
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
ENTRYPOINT ["/app/trader"]
CMD ["daemon"]
