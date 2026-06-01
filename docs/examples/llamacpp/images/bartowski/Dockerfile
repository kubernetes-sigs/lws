# syntax=docker/dockerfile:1
FROM debian:bullseye AS quantizer

ARG BLOB_SERVER

RUN apt-get update && apt-get install --yes curl iproute2

RUN mkdir -p /models/bartowski/Meta-Llama-3-8B-Instruct-GGUF
WORKDIR /models/bartowski/Meta-Llama-3-8B-Instruct-GGUF
# Pulling from our blobstore is much faster, even than copying from docker host
RUN if [ -n "$BLOB_SERVER" ]; then \
    curl -v -L --fail -o Meta-Llama-3-8B-Instruct-Q5_K_M.gguf http://$BLOB_SERVER:9999/16d824ee771e0e33b762bb3dc3232b972ac8dce4d2d449128fca5081962a1a9e; \
  else \
    curl -v -L --fail -o Meta-Llama-3-8B-Instruct-Q5_K_M.gguf http://$(ip -4 route show default | awk '/default/ { print $3 }'):9999/16d824ee771e0e33b762bb3dc3232b972ac8dce4d2d449128fca5081962a1a9e; \
  fi
RUN sha256sum /models/bartowski/Meta-Llama-3-8B-Instruct-GGUF/Meta-Llama-3-8B-Instruct-Q5_K_M.gguf

FROM llamacpp-leader

# TODO: This copy command is a bottleneck (it doesn't seem to be reliably cached?)
# Maybe we create a simple download tool and embed it the llama-server image.
COPY --from=quantizer /models /models

ENV LLM_MODEL /models/bartowski/Meta-Llama-3-8B-Instruct-GGUF/Meta-Llama-3-8B-Instruct-Q5_K_M.gguf

ENTRYPOINT ["/llama-server", "--model", "/models/bartowski/Meta-Llama-3-8B-Instruct-GGUF/Meta-Llama-3-8B-Instruct-Q5_K_M.gguf"]