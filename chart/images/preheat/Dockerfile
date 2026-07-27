FROM golang:1.23-alpine AS builder

RUN CGO_ENABLED=0 go install github.com/fullstorydev/grpcurl/cmd/grpcurl@v1.9.3

FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates jq

COPY --from=builder /go/bin/grpcurl /usr/local/bin/grpcurl

ENTRYPOINT ["/bin/bash", "-c"]
