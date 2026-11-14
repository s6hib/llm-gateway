# Build the gateway, then ship an image that holds only the program and the
# list of certificate authorities it needs to call the API over HTTPS.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gateway .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /gateway /gateway
ENTRYPOINT ["/gateway"]
