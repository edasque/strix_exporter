FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /strix_exporter .

FROM alpine:3.21
COPY --from=build /strix_exporter /usr/bin/strix_exporter
EXPOSE 9101
ENTRYPOINT ["/usr/bin/strix_exporter"]
