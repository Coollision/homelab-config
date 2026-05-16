FROM amazon/aws-cli:2.34.48
WORKDIR /
ARG TARGETARCH
COPY dist/manager-linux-${TARGETARCH} /manager
RUN chmod +x /manager
EXPOSE 8080 8081 8090
ENTRYPOINT ["/manager"]
