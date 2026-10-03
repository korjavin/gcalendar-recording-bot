FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gcalendar-recording-bot . && mkdir /out/data

# distroless/static ships CA certificates and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gcalendar-recording-bot /gcalendar-recording-bot
# The state directory, owned by nonroot (65532); a fresh named volume mounted
# here inherits that ownership.
COPY --from=build --chown=65532:65532 /out/data /data
ENV LISTEN_ADDR=:8080 DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/gcalendar-recording-bot"]
