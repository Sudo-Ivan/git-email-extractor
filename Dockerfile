FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY main.go go.mod go.sum ./
RUN go build -o git-email-extractor

FROM alpine:latest
LABEL org.opencontainers.image.source="https://github.com/Sudo-Ivan/Git-Email-Extractor"
LABEL org.opencontainers.image.description="A tool to extract email addresses from git repositories"
LABEL org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache git
COPY --from=builder /app/git-email-extractor /usr/local/bin/
ENTRYPOINT ["git-email-extractor"] 