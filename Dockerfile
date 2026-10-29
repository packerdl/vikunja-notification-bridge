FROM dhi.io/golang:1.27-alpine3.24-dev AS build
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/bridge .

FROM dhi.io/alpine-base:3.24
COPY --from=build /out/bridge /bridge
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV LISTEN_ADDR=":8080" TZ=UTC
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/bridge", "healthcheck"]
ENTRYPOINT ["/bridge"]
