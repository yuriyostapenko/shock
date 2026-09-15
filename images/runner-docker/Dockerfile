# syntax=docker/dockerfile:1.7
# EXPERIMENTAL: the default runner image plus a rootless Docker engine that a
# session can start itself with `dockerd-rootless.sh &` (DOCKER_HOST already
# points at its socket). Verified locally with docker run under the settings
# below; untested on Kubernetes and outside the release pipeline.
#
# The engine runs as the runner user (uid 1000) through rootlesskit. The Pod
# running this image needs, on the runner container:
#   securityContext.seccompProfile.type: Unconfined
#   securityContext.appArmorProfile.type: Unconfined
#   securityContext.procMount: Unmasked
#   securityContext.allowPrivilegeEscalation: true   (newuidmap file capabilities)
#   securityContext.capabilities: {drop: [ALL], add: [SETUID, SETGID]}
#   a /dev/net/tun device (slirp4netns), e.g. through a device plugin
#   a volume owned by uid 1000 at /home/runner/.local/share/docker (emptyDir
#   with fsGroup 1000, or the workspace PVC); overlayfs cannot nest on the
#   image filesystem.
# Unconfined seccomp and an unmasked /proc place the namespace at the
# privileged Pod Security Standards level.

ARG BASE_IMAGE=shock-runner:dev
ARG DOCKER_VERSION=29.8.0

FROM ${BASE_IMAGE}
ARG DOCKER_VERSION TARGETARCH
USER 0
RUN apt-get update \
 && apt-get install -y --no-install-recommends iproute2 iptables libcap2-bin procps slirp4netns uidmap \
 && rm -rf /var/lib/apt/lists/* \
 && case "${TARGETARCH}" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac \
 && curl --retry 5 --retry-delay 2 -fsSL "https://download.docker.com/linux/static/stable/${arch}/docker-${DOCKER_VERSION}.tgz" | tar -xz -C /tmp \
 && curl --retry 5 --retry-delay 2 -fsSL "https://download.docker.com/linux/static/stable/${arch}/docker-rootless-extras-${DOCKER_VERSION}.tgz" | tar -xz -C /tmp \
 && install -m 0755 /tmp/docker/* /tmp/docker-rootless-extras/* /usr/local/bin/ \
 && rm -rf /tmp/docker /tmp/docker-rootless-extras \
 # newuidmap/newgidmap gain their capability from the file, as the docker:dind-rootless image ships them
 && chmod u-s /usr/bin/newuidmap /usr/bin/newgidmap \
 && /usr/sbin/setcap cap_setuid+ep /usr/bin/newuidmap \
 && /usr/sbin/setcap cap_setgid+ep /usr/bin/newgidmap \
 && echo "runner:100000:65536" >> /etc/subuid \
 && echo "runner:100000:65536" >> /etc/subgid \
 && mkdir -p /run/user/1000 && chown 1000:1000 /run/user/1000
USER 1000
ENV PATH=/home/runner/.local/share/mise/shims:/home/runner/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    XDG_RUNTIME_DIR=/run/user/1000 \
    DOCKER_HOST=unix:///run/user/1000/docker.sock \
    DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns
