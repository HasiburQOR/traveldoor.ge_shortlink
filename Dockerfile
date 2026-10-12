# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shortlink .

# ---- run ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir /data && chown app /data
USER app
ENV PORT=8080 DB_PATH=/data/links.db
EXPOSE 8080
COPY --from=build /out/shortlink /usr/local/bin/shortlink
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:$PORT/healthz || exit 1
ENTRYPOINT ["shortlink"]
