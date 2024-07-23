# syntax=docker/dockerfile:1
FROM golang:latest AS builder

ADD tools/clichat /workspace/clichat
WORKDIR /workspace/clichat

RUN CGO_ENABLED=0 go build -o /clichat ./cmd/clichat

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /clichat /clichat

ENTRYPOINT ["/clichat"]
