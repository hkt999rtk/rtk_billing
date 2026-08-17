FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/rtk-billing ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -o /out/rtk-billing-payment-worker ./cmd/payment-worker \
    && CGO_ENABLED=0 go build -trimpath -o /out/rtk-billing-payment-simulator ./cmd/payment-simulator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rtk-billing /rtk-billing
COPY --from=build /out/rtk-billing-payment-worker /rtk-billing-payment-worker
COPY --from=build /out/rtk-billing-payment-simulator /rtk-billing-payment-simulator
EXPOSE 8080
ENTRYPOINT ["/rtk-billing"]
