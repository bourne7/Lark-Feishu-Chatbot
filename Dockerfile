FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /lark-feishu-chatbot ./cmd/lark-feishu-chatbot

# An empty runtime contains no shell, package manager or obsolete OS packages.
FROM scratch
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /lark-feishu-chatbot /lark-feishu-chatbot
COPY LICENSE /licenses/LICENSE
COPY licenses/ /licenses/
ENTRYPOINT ["/lark-feishu-chatbot"]
