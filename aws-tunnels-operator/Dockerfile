FROM golang:1.22 AS builder
WORKDIR /src
COPY go.mod .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/manager ./

FROM amazon/aws-cli:2.27.9
WORKDIR /
COPY --from=builder /out/manager /manager
EXPOSE 8080 8081 8090
ENTRYPOINT ["/manager"]
