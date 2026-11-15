# Tools-only image for the cert-push CronJob (GitOps repo: system/default/cert-push).
# The push script is NOT baked in: it ships as a ConfigMap in the GitOps repo so it is reviewable there.
# Keep this image boring: only what the script calls, no shell tooling for poking around, non-root.
FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

# bash/curl/openssl/jq/ca-certificates are everything push.sh needs (awk, date, timeout come from busybox).
# apk is removed afterwards so a compromised pod cannot install tooling at runtime.
RUN apk add --no-cache bash curl openssl jq ca-certificates \
 && addgroup -g 65532 -S certpush \
 && adduser -u 65532 -S -G certpush -h /tmp -s /sbin/nologin certpush \
 && rm -f /sbin/apk \
 && rm -rf /var/cache/apk /etc/apk/cache

USER 65532:65532
WORKDIR /tmp
ENTRYPOINT ["/bin/bash"]
