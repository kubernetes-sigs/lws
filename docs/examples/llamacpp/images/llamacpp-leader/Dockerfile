# syntax=docker/dockerfile:1
FROM debian:bullseye AS builder

RUN apt-get update && apt-get install --yes git cmake g++

RUN mkdir /workspace
WORKDIR /workspace
RUN git clone https://github.com/ggerganov/llama.cpp.git

WORKDIR /workspace/llama.cpp

# GGML_RPC=ON: Builds RPC support
# BUILD_SHARED_LIBS=OFF: Don't rely on shared libraries like libggml
RUN cmake . -DGGML_RPC=ON -DBUILD_SHARED_LIBS=OFF -DGGML_DEBUG=1 -DLLAMA_CURL=OFF
RUN cmake --build . --config Release --parallel 8


FROM golang:latest AS gobuilder

ADD tools/llamacpp-leader /workspace/llamacpp-leader
WORKDIR /workspace/llamacpp-leader

RUN CGO_ENABLED=0 go build -o /llamacpp-leader ./cmd/llamacpp-leader


FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=builder /workspace/llama.cpp/bin/llama-server /llama-server
COPY --from=gobuilder /llamacpp-leader /llamacpp-leader

ENTRYPOINT ["/llamacpp-leader"]
