# Build stage: compile a static-ish relay binary.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO is not needed: the SQLite driver (modernc.org/sqlite) is pure Go.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/lanmsg-server ./cmd/lanmsg-server

# Runtime stage: minimal image.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/lanmsg-server /usr/local/bin/lanmsg-server

# Config + data live on a mounted volume at /data.
VOLUME ["/data"]
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/lanmsg-server"]
CMD ["run", "-config", "/data/server.toml"]
