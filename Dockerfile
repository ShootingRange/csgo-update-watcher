FROM golang:1.17

WORKDIR /go/src/csgo-update-watcher
COPY . .

RUN go install -v ./...

CMD ["csgo-update-watcher"]