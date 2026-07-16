FROM alpine:3.22

RUN apk add --no-cache ca-certificates
COPY trec /usr/local/bin/trec

ENTRYPOINT ["/usr/local/bin/trec"]
