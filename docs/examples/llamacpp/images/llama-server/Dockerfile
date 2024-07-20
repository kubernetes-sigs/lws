FROM debian:bullseye AS builder

RUN apt-get update && apt-get install --yes git make g++

RUN mkdir /workspace
WORKDIR /workspace
RUN git clone https://github.com/ggerganov/llama.cpp.git

WORKDIR /workspace/llama.cpp


RUN make -j8

FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=builder /workspace/llama.cpp/llama-server /llama-server

ENTRYPOINT ["/llama-server"]
