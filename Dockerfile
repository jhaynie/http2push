FROM alpine:latest

ARG VERSION
ARG COMMITSHA

LABEL "io.pinpt.build.commit=${COMMITSHA}" "io.pinpt.build.version=${VERSION}"

COPY build/alpine/http2push-alpine-"${VERSION}" /app/http

CMD ["/app/http"]
ENTRYPOINT ["/app/http"]
