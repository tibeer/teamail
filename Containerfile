##########################################
# Build step
##########################################
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN apk add --no-cache ca-certificates && CGO_ENABLED=0 go build -o teamail .

##########################################
# Minimal teamail container
##########################################
FROM scratch
COPY --from=build /src/teamail /teamail
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["/teamail"]
