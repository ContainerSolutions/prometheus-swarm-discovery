FROM golang:1.7-alpine

RUN mkdir /app

RUN echo http://dl-cdn.alpinelinux.org/alpine/edge/community >> /etc/apk/repositories
#Install glide and git
RUN apk add --no-cache glide git

COPY . /go/src/github.com/weaveworks/prometheus-swarm/

WORKDIR /go/src/github.com/weaveworks/prometheus-swarm/

#run glide install, compile go binary, then remove /vendor directory used by glide and .glide cache
RUN glide install &&\
    CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/main /go/src/github.com/weaveworks/prometheus-swarm/swarm.go &&\
    rm -rf vendor/ /root/.glide/

ENTRYPOINT ["/app/main", "discover"]
