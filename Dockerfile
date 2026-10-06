# Built natively on the image's own architecture. Alpine's toolchain is musl,
# so the binary is static and runs on distroless/static.
FROM rust:1.97-alpine AS build
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY . .
RUN cargo build --release --locked && cp target/release/bambu-util /bambu-util

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bambu-util /bambu-util
EXPOSE 8081
ENTRYPOINT ["/bambu-util"]
