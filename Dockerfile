FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/mantlerd ./cmd/mantler

FROM alpine:3.20
RUN addgroup -S claw && adduser -S claw -G claw
USER claw
COPY --from=builder /out/mantlerd /usr/local/bin/mantlerd
ENTRYPOINT ["/usr/local/bin/mantlerd"]
