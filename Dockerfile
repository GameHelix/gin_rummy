FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
# COMMIT is the source commit, reported by /v1/health; deploy.sh sets it.
ARG COMMIT=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY api ./api
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.commit=$COMMIT" -o /out/trader ./cmd/trader

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/trader /app/trader
ENTRYPOINT ["/app/trader"]
CMD ["serve"]
