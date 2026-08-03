# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# Binario estático, sin CGO, optimizado en tamaño.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /proxy .

# ---- runtime ----
# distroless/static: sin shell, sin gestor de paquetes, mínima superficie de ataque.
FROM gcr.io/distroless/static-debian12
COPY --from=build /proxy /proxy
# Variables por defecto (sobrescribibles en EasyPanel).
ENV PORT=8080
ENV UPSTREAM_URL=https://api.anthropic.com
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/proxy", "-health"]
ENTRYPOINT ["/proxy"]
