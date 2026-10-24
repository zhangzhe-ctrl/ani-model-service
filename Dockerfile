# Runtime image for the Model control plane. Build the service from this
# repository so the Helm chart can deploy an immutable image for the target
# cluster without depending on a developer workstation binary.
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.Name=ani-model-service" \
    -o /out/ani-model-service ./cmd/ani-model-service
RUN mkdir -p /out/configs \
    && cp configs/config.yaml /out/configs/config.yaml \
    && chmod 0555 /out/configs \
    && chmod 0444 /out/configs/config.yaml

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /out/ani-model-service /ani-model-service
COPY --from=builder /out/configs /configs

USER 65532:65532
ENTRYPOINT ["/ani-model-service"]
