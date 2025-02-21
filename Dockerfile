FROM golang:1.20-alpine as builder
MAINTAINER FullStory Engineering

# create non-privileged group and user
RUN addgroup -S grpcurl && adduser -S grpcurl -G grpcurl

WORKDIR /tmp/tetrateio/grpcurl
# copy just the files/sources we need to build grpcurl
COPY VERSION *.go go.* /tmp/tetrateio/grpcurl/
COPY cmd /tmp/tetrateio/grpcurl/cmd
# and build a completely static binary (so we can use
# scratch as basis for the final image)
ENV CGO_ENABLED=0
ENV GO111MODULE=on
RUN go build -o /grpcurl \
    -ldflags "-w -extldflags \"-static\" -X \"main.version=$(cat VERSION)\"" \
    ./cmd/grpcurl

FROM alpine:3 as alpine
RUN apk update && apk upgrade
WORKDIR /
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /grpcurl /bin/grpcurl
USER grpcurl

ENTRYPOINT ["/bin/grpcurl"]

# New FROM so we have a nice'n'tiny image
FROM scratch
WORKDIR /
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /grpcurl /bin/grpcurl
USER grpcurl

ENTRYPOINT ["/bin/grpcurl"]
