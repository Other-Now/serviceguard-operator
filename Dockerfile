# One image, two binaries: /manager (the operator) and /demoapp (the e2e chaos target).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demoapp ./cmd/demoapp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /out/demoapp /
USER 65532:65532
ENTRYPOINT ["/manager"]
