FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bili-acc ./cmd/bili-acc && mkdir /traces

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /bili-acc /bili-acc
COPY --from=build --chown=65532:65532 /traces /traces
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/bili-acc"]
