FROM golang:1.7-alpine AS builder
COPY . /go/src/github.com/weaveworks/prometheus-swarm/
RUN echo "http://dl-cdn.alpinelinux.org/alpine/edge/community" >> /etc/apk/repositories \
    && apk --update add glide git \
    && cd /go/src/github.com/weaveworks/prometheus-swarm/ \
    && mkdir /app \
    && glide install \
    && CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/main /go/src/github.com/weaveworks/prometheus-swarm/swarm.go

FROM alpine:3.6
COPY --from=builder /app/main /promswarm
ENTRYPOINT ["/promswarm", "discover"]
