FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/rtk-billing ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rtk-billing /rtk-billing
EXPOSE 8080
ENTRYPOINT ["/rtk-billing"]
