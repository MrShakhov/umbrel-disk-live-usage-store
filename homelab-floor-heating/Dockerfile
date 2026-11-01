# Cross-compiles on the build host (fast even for arm64), ships a static binary.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/floor-heating .

FROM scratch
COPY --from=build /out/floor-heating /floor-heating
EXPOSE 8080
USER 1000:1000
ENTRYPOINT ["/floor-heating"]
