# Context: manager/. Build only the core binaries and CA roots.
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/manager ./cmd/oc-provision ./cmd/oc-dockerproxy ./cmd/render-instance

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/ /
EXPOSE 8080 4000 9100
ENTRYPOINT ["/manager"]
