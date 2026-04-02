FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/clawcontrol-agent ./cmd/clawcontrol-agent

FROM alpine:3.20
RUN addgroup -S claw && adduser -S claw -G claw
USER claw
COPY --from=builder /out/clawcontrol-agent /usr/local/bin/clawcontrol-agent
ENTRYPOINT ["/usr/local/bin/clawcontrol-agent"]
