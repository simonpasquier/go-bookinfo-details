FROM alpine:latest

RUN apk --update add ca-certificates

COPY ./details /
ENTRYPOINT [ "/details" ]
