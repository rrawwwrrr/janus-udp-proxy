FROM golang:1.25 as builder
RUN apt-get update && apt-get -y install unzip wget curl git
WORKDIR /app
COPY . .

RUN go build -o proxy


FROM ubuntu:24.04

#RUN apt update && apt -y install unzip wget curl libimobiledevice-utils libimobiledevice6 usbmuxd iproute2 net-tools \
#        curl git build-essential libssl-dev zlib1g-dev socat && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/proxy /app/proxy
RUN chmod +x /app/proxy

ENTRYPOINT ["/app/proxy"]