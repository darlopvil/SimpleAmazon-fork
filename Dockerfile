# syntax=docker/dockerfile:1

# ---------- build ----------
FROM golang:1.26-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# Capa cacheada: solo se rehace si cambian las dependencias
COPY go.mod go.sum ./
RUN go mod download

# templates/ y static/ van embebidos en el binario con //go:embed
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /simpleamazon .

# ---------- runtime ----------
FROM scratch

# Necesarios para el TLS de salida hacia Amazon
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /simpleamazon /simpleamazon

USER 65534:65534
EXPOSE 8080

# El binario escucha en 0.0.0.0:8080 por defecto. Se puede ajustar con las
# variables SIMPLEAMAZON_HOST y SIMPLEAMAZON_PORT, o con los flags -h y -p.
ENTRYPOINT ["/simpleamazon"]
