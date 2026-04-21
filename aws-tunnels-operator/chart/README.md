# aws-tunnels-operator Helm Chart

This chart deploys the operator and can optionally create one or more `AWSTunnelStack` custom resources from values.

## Install

```bash
helm upgrade --install aws-tunnels-operator \
  tests/proxies/aws-tunnels-operator/chart \
  -n proxies --create-namespace
```

## Enable stack creation from values

Set `stacks` in values:

```yaml
stacks:
  - name: aws-tunnels
    spec:
      aws:
        profile: awsprofile001
        region: eu-west-1
        ssoStartUrl: <secret:kv/data/aws~sso-start-url>
        accountId: <secret:kv/data/aws~account-id>
        roleName: Admin
      auth:
        enabled: true
        host: aws-auth.<secret:kv/data/domains~domain>
        port: 8090
      tunnelDefaults:
        image: ghcr.io/coollision/aws-tunnels-operator:v0.1.0
        proxyImage: ubuntu/squid:5.2-22.04_beta
        servicePort: 3128
      tunnels:
        - name: gitlab-dev
          host: gitlab-dev.<secret:kv/data/domains~domain>
          bastionName: bhnonprod
          remoteHost: localhost
          remotePort: "8181"
          localPort: 8181
          ingressMode: http
```

## API entrypoint

Auth endpoint (inside operator pod):

`POST /login`

Body:

```json
{"namespace":"proxies","stack":"aws-tunnels","profile":"awsprofile001"}
```
