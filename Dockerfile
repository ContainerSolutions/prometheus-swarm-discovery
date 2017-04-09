FROM golang:1.7-alpine

RUN mkdir /app
COPY . /go/src/github.com/weaveworks/prometheus-swarm/

RUN echo http://dl-cdn.alpinelinux.org/alpine/edge/community >> /etc/apk/repositories
RUN apk update && apk add glide git
RUN cd /go/src/github.com/weaveworks/prometheus-swarm/ && glide install

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/main /go/src/github.com/weaveworks/prometheus-swarm/swarm.go

ENTRYPOINT ["/app/main", "discover"]
