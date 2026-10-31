FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
ARG TARGETARCH
COPY dist/manager-linux-${TARGETARCH} /manager
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/manager"]
