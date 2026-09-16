FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/llamacpp-affinity-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llamacpp-affinity-proxy /llamacpp-affinity-proxy
EXPOSE 8001
ENTRYPOINT ["/llamacpp-affinity-proxy"]

LABEL org.opencontainers.image.source="https://github.com/JochenLinnemann/llamacpp-affinity-proxy"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Dynamic conversation-to-slot affinity proxy for llama.cpp"
