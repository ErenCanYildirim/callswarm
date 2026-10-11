# The Jargo benchmark bot. Mirrors Jargo's own base images (docker/ in the
# Jargo repository, v0.2.0): the same pinned images and ONNX Runtime release,
# without RNNoise, which the bot does not use.

FROM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod main.go ./
RUN go mod tidy && go build -ldflags="-s -w" -o /out/bot .

FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS ort
ARG ORT_VERSION=1.30.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-x64-${ORT_VERSION}.tgz" \
    && mkdir -p /opt/ort && tar -xzf /tmp/ort.tgz -C /opt/ort --strip-components=1 \
    && cp -L /opt/ort/lib/libonnxruntime.so /usr/local/lib/libonnxruntime.so

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985
COPY --from=ort /usr/local/lib/libonnxruntime.so /usr/local/lib/libonnxruntime.so
ENV JARGO_ONNXRUNTIME_LIB=/usr/local/lib/libonnxruntime.so
COPY --from=build /out/bot /usr/local/bin/bot
EXPOSE 8765
ENTRYPOINT ["/usr/local/bin/bot", "-addr", "0.0.0.0:8765"]