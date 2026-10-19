FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27.2-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=frontend /src/web/out/ internal/web/dist/
RUN CGO_ENABLED=0 go build -trimpath -o /tunnel ./cmd/tunnel

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -S tunnel && adduser -S -G tunnel tunnel && mkdir -p /var/lib/tunnel && chown tunnel:tunnel /var/lib/tunnel
WORKDIR /app
COPY --from=backend /tunnel /app/tunnel
USER tunnel
EXPOSE 8080
ENTRYPOINT ["/app/tunnel"]
