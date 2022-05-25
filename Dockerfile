FROM golang:alpine AS builder

RUN mkdir /build
WORKDIR /build

# git is required for fetching the dependencies.
# gcc is required for confluent-kafka-go
# libc-dev for stdio.h libraries
RUN apk update && apk add --no-cache git gcc libc-dev

COPY release-changelog.go .
COPY go.mod .
COPY go.sum .

# Fetch dependencies.
RUN go get -d -v

# Build statically so all libaries are built into the binary.
RUN go build -tags musl -ldflags="-extldflags=-static" release-changelog.go

# Use scratch image to host the binary
FROM scratch

ARG BOOTSTRAP_SERVERS

ENV KAFKA_BOOTSTRAP_SERVERS=${BOOTSTRAP_SERVERS}

# Copy our static executable.
COPY --from=builder /build/release-changelog /

# Run the release-changelog binary.
ENTRYPOINT ["/release-changelog"]
