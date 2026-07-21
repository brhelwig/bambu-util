FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /p1s-bridge ./cmd/p1s-bridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /p1s-bridge /p1s-bridge
EXPOSE 8081
ENTRYPOINT ["/p1s-bridge"]
