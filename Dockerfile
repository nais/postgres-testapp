FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /postgres-testapp .

FROM scratch
COPY --from=build /postgres-testapp /postgres-testapp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/postgres-testapp"]
