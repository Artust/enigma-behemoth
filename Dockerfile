# --- build stage ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- runtime stage ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
