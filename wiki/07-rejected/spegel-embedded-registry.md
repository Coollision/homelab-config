# Rejected — k3s embedded registry (Spegel)

[← Back to Rejected ideas](README.md) · Chosen instead: [Registry mirror](../06-todo/registry-mirror.md)

> **Status (2026-11-03):** rejected on reasoning, never enabled or tested on a node.

## The idea

Enable the k3s `embedded-registry` server flag plus endpoint-less `mirrors:` entries in `registries.yaml`, so
nodes serve images they already hold to each other over the LAN. Hoped-for bonus: make kube-fledged
unnecessary.

## Why it lost

- Spegel only shares images **some live node already holds**. It never pushes an image to the node that will run
  the pod, and it has no pre-stage mechanism.
- The case that matters is a node dying. The node that ran the pod is the only one holding the image, and it is
  gone, so there is nothing to serve; the survivor pulls from the internet exactly as today (~85 s for the
  Home Assistant image).
- It does not help when the internet is unreachable, and it does nothing for a first pull of a new image.
- It cannot replace kube-fledged: copying what kube-fledged already placed on each node saves nothing.
- Enabling it is a server-side flag plus a k3s restart on every node (`KillMode=control-group` kills all pods),
  for a benefit limited to maintenance drains and joining nodes.

## What to do instead

A node-independent pull-through mirror on the NAS: see [Registry mirror](../06-todo/registry-mirror.md).

## Reopen if

Pulls between nodes become the bottleneck while the NAS mirror is unavailable, or the cluster grows enough that
a mirror per upstream is too much to run.
