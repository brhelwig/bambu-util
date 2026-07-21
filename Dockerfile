FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /bambu-util ./cmd/bambu-util

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bambu-util /bambu-util
EXPOSE 8081
ENTRYPOINT ["/bambu-util"]
