FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hello .

FROM scratch
COPY --from=build /hello /hello
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/hello"]
