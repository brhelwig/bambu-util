# zig and cargo-zigbuild come from PyPI, where both are published as wheels,
# and are copied into the Rust stage below.
FROM --platform=$BUILDPLATFORM python:3.13-slim AS zig
COPY .github/zigbuild/requirements.txt /tmp/zigbuild.txt
RUN pip install --no-cache-dir --target /opt/zigbuild -r /tmp/zigbuild.txt

FROM --platform=$BUILDPLATFORM rust:1.97-bookworm AS build
ARG TARGETARCH
COPY --from=zig /opt/zigbuild /opt/zigbuild
RUN ln -s /opt/zigbuild/ziglang/zig /usr/local/bin/zig && \
    ln -s /opt/zigbuild/bin/cargo-zigbuild /usr/local/bin/cargo-zigbuild
WORKDIR /src
COPY . .
# Statically linked against musl, so it runs on distroless/static.
RUN case "$TARGETARCH" in \
      amd64) target=x86_64-unknown-linux-musl ;; \
      arm64) target=aarch64-unknown-linux-musl ;; \
      *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    rustup target add "$target" && \
    cargo zigbuild --release --locked --target "$target" && \
    cp "target/$target/release/bambu-util" /bambu-util

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bambu-util /bambu-util
EXPOSE 8081
ENTRYPOINT ["/bambu-util"]
