# ymcp as a Streamable HTTP server. Profiles live in the /config volume;
# without a keychain in the container their passwords are kept in that file.
#
#   docker run -it --rm -v ymcp:/config xlsoftware/ymcp auth
#   docker run -d -p 8090:8090 -v ymcp:/config -e YMCP_HTTP_TOKEN=… xlsoftware/ymcp

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION#v}" -o /out/ymcp ./cmd/ymcp
RUN mkdir -p /out/config

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ymcp /usr/local/bin/ymcp
COPY --from=build --chown=nonroot:nonroot /out/config /config
ENV YMCP_CONFIG=/config/config.json \
    YMCP_LOG_FILE=off \
    YMCP_INSTALL=docker
VOLUME /config
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=6s --start-period=5s CMD ["ymcp", "health"]
ENTRYPOINT ["ymcp"]
CMD ["serve", "--transport", "http", "--addr", "0.0.0.0:8090"]
