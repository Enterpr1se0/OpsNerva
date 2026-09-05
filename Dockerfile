FROM node:26-alpine AS web
WORKDIR /src/web
RUN npm install --global pnpm@11.22.0
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build

FROM golang:1.26-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/opsnerva ./cmd/opsnerva

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bubblewrap && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=backend /out/opsnerva /app/opsnerva
COPY configs ./configs
VOLUME ["/app/data"]
EXPOSE 8080
CMD ["./opsnerva", "serve"]
