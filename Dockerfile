FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gcalendar-recording-bot .

# distroless/static ships CA certificates and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gcalendar-recording-bot /gcalendar-recording-bot
EXPOSE 8080
ENTRYPOINT ["/gcalendar-recording-bot"]
