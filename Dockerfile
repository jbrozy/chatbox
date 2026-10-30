# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/chatbox ./cmd/chatbox

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/chatbox /chatbox
ENV PORT=3000
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/chatbox"]
