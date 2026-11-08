FROM amazon/aws-cli:2.39.6
WORKDIR /
ARG TARGETARCH
COPY dist/manager-linux-${TARGETARCH} /manager
RUN chmod +x /manager
# Non-root; matches the pod securityContext in the chart.
USER 65532:65532
EXPOSE 8080 8081 8090
ENTRYPOINT ["/manager"]
