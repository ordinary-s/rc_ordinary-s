ARG GO_IMAGE=golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
ARG RUNTIME_IMAGE=debian:13.1-slim@sha256:a347fd7510ee31a84387619a492ad6c8eb0af2f2682b916ff3e643eb076f925a
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mock-target ./cmd/mock-target
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/devcert ./cmd/devcert
FROM ${GO_IMAGE} AS test-runner
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["bash", "/src/scripts/verify-in-container.sh"]
FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /app
COPY --from=build --chown=65532:65532 /out/server /app/server
COPY --from=build --chown=65532:65532 /out/migrate /app/migrate
COPY --from=build --chown=65532:65532 /out/mock-target /app/mock-target
COPY --from=build --chown=65532:65532 /out/healthcheck /app/healthcheck
COPY --chown=65532:65532 migrations /app/migrations
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/app/server"]

FROM ${RUNTIME_IMAGE} AS cert-init
WORKDIR /app
COPY --from=build /out/devcert /app/devcert
USER 0:0
ENTRYPOINT ["/bin/sh", "-c"]
