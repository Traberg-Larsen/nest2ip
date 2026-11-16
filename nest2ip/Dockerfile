FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go status.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=0.1.6" -o /nest2ip .

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /nest2ip /usr/local/bin/nest2ip
ENV TZ=Europe/Copenhagen
CMD ["/usr/local/bin/nest2ip"]
