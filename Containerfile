FROM docker.io/library/golang:1.24-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./...
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/phnctl ./cmd/phnctl

FROM scratch
COPY --from=build /out/phnctl /phnctl
USER 1000:1000
EXPOSE 8443
ENTRYPOINT ["/phnctl"]
CMD ["serve"]

