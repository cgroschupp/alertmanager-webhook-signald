FROM golang:1.16 as build-env

WORKDIR /go/src/app
ADD . /go/src/app

RUN go get -d -v ./...

RUN go build -o /go/bin/alertmanager-webhook-signald

FROM gcr.io/distroless/base
COPY --from=build-env /go/bin/alertmanager-webhook-signald /
ENTRYPOINT ["/alertmanager-webhook-signald"]