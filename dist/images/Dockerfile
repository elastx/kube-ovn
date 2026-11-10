# syntax=docker/dockerfile:1
ARG VERSION
ARG BASE_TAG=$VERSION
ARG GO_VERSION=1.27.3
FROM kubeovn/kube-ovn-base:$BASE_TAG AS setcap

COPY *.sh /kube-ovn/
COPY kubectl-ko /kube-ovn/kubectl-ko
COPY 01-kube-ovn.conflist /kube-ovn/01-kube-ovn.conflist

COPY kube-ovn /kube-ovn/kube-ovn
COPY kube-ovn-cmd /kube-ovn/kube-ovn-cmd
COPY kube-ovn-bfdd-supervisor /kube-ovn/kube-ovn-bfdd-supervisor
COPY kube-ovn-daemon /kube-ovn/kube-ovn-daemon
COPY kube-ovn-controller /kube-ovn/kube-ovn-controller
COPY vpc-egress-gateway-observer /kube-ovn/vpc-egress-gateway-observer
RUN ln -s /kube-ovn/kube-ovn-cmd /kube-ovn/kube-ovn-monitor && \
    ln -s /kube-ovn/kube-ovn-cmd /kube-ovn/kube-ovn-speaker && \
    ln -s /kube-ovn/kube-ovn-cmd /kube-ovn/kube-ovn-webhook && \
    ln -s /kube-ovn/kube-ovn-cmd /kube-ovn/kube-ovn-leader-checker && \
    ln -s /kube-ovn/kube-ovn-cmd /kube-ovn/kube-ovn-ic-controller && \
    ln -s /kube-ovn/kube-ovn /kube-ovn/kube-ovn-acl-sample && \
    ln -s /kube-ovn/kube-ovn-controller /kube-ovn/kube-ovn-pinger && \
    setcap CAP_NET_BIND_SERVICE+eip /kube-ovn/kube-ovn-cmd && \
    setcap CAP_NET_RAW,CAP_NET_BIND_SERVICE+eip /kube-ovn/kube-ovn-controller && \
    setcap CAP_NET_ADMIN,CAP_NET_RAW,CAP_NET_BIND_SERVICE,CAP_SYS_ADMIN+eip /kube-ovn/kube-ovn-daemon && \
    setcap CAP_NET_ADMIN+eip /kube-ovn/vpc-egress-gateway-observer

FROM golang:$GO_VERSION-alpine AS x-deps

ARG KUBECTL_VERSION=v1.35.8
ARG GOBGP_VERSION=4.7.0
ARG X_NET_VERSION=v0.62.0
ARG X_TEXT_VERSION=v0.43.0
ARG GRPC_VERSION=v1.83.2
RUN apk --no-cache add bash curl
COPY go-deps/bump-x-deps.sh /
RUN bash /bump-x-deps.sh

FROM kubeovn/kube-ovn-base:$BASE_TAG

COPY --chmod=0644 logrotate/* /etc/logrotate.d/
COPY grace_stop_ovn_controller /usr/share/ovn/scripts/grace_stop_ovn_controller

COPY --from=setcap /kube-ovn /kube-ovn
COPY --from=x-deps /out/gobgp /out/kubectl /usr/bin/
RUN /kube-ovn/iptables-wrapper-installer.sh --no-sanity-check

WORKDIR /kube-ovn
