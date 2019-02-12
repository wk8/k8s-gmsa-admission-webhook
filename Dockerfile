## Dockerfile for release, as lightweight as possible

ARG GO_VERSION
FROM golang:${GO_VERSION} AS builder

WORKDIR /go/src/github.com/wk8/k8s-gmsa-admission-webhook

# install go dependencies
RUN curl https://glide.sh/get | sh
COPY glide.* ./
RUN glide install -v

# build
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s"

###

FROM scratch

WORKDIR /webhook

ENV LOG_LEVEL=info

COPY --from=builder /go/src/github.com/wk8/k8s-gmsa-admission-webhook/k8s-gmsa-admission-webhook .

ENTRYPOINT ["/webhook/k8s-gmsa-admission-webhook"]
