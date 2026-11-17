# CI image with Go, HAProxy, Docker CLI, and Kubernetes tools
# Every version ARG is supplied from ci/build-pins.yml (scripts/build-ci-image.sh)
#
# Uses haproxytech/haproxy-debian as the base image so HAProxy is natively
# available for all versions without depending on haproxy.debian.net.
# Go toolchain is copied from the official golang image.

ARG GO_BUILD_IMAGE=golang:must-be-supplied@sha256:0000000000000000000000000000000000000000000000000000000000000000
ARG HAPROXY_IMAGE=haproxytech/haproxy-debian:must-be-supplied@sha256:0000000000000000000000000000000000000000000000000000000000000000
ARG NODE_BUILD_IMAGE=node:must-be-supplied@sha256:0000000000000000000000000000000000000000000000000000000000000000

# Stage 1: Go toolchain source
FROM ${GO_BUILD_IMAGE} AS go-toolchain

FROM ${NODE_BUILD_IMAGE} AS node-toolchain

# Stage 2: CI image based on official HAProxy image
FROM ${HAPROXY_IMAGE}

# Copy Go toolchain from official image
COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY --from=node-toolchain /usr/local/bin/node /usr/local/bin/node
COPY --from=node-toolchain /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
ENV GOPATH=/go \
    PATH="/usr/local/go/bin:/go/bin:${PATH}"
RUN mkdir -p "$GOPATH/src" "$GOPATH/bin" && chmod -R 1777 "$GOPATH"

ARG DOCKER_STATIC_VERSION
ARG DOCKER_STATIC_SHA256
ARG DOCKER_BUILDX_VERSION
ARG DOCKER_BUILDX_SHA256
ARG DEBIAN_SNAPSHOT
ARG KIND_VERSION
ARG CT_VERSION
ARG HELM_UNITTEST_VERSION
ARG KUBECONFORM_VERSION
ARG YAMLLINT_VERSION
ARG YAMALE_VERSION
ARG MARKDOWNLINT_VERSION
ARG VALE_VERSION
ARG GORELEASER_VERSION
ARG COSIGN_VERSION
ARG SYFT_VERSION
ARG HELM_VERSION
ARG HELM_MIN_VERSION
ARG KUBECTL_VERSION
ARG YQ_VERSION
ARG GOCOVER_COBERTURA_VERSION
ARG VALE_SHA256
ARG KIND_SHA256
ARG KUBECTL_SHA256
ARG HELM_SHA256
ARG HELM_MIN_SHA256
ARG YQ_SHA256
ARG CT_SHA256
ARG HELM_UNITTEST_SHA256
ARG KUBECONFORM_SHA256
ARG GORELEASER_SHA256
ARG COSIGN_SHA256
ARG SYFT_SHA256

# Install build dependencies from one immutable Debian repository state.
RUN . /etc/os-release \
    && rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* \
    && printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/%s %s main\n' \
       "${DEBIAN_SNAPSHOT}" "${VERSION_CODENAME}" >/etc/apt/sources.list \
    && echo 'Acquire::Retries "8";' >/etc/apt/apt.conf.d/80-snapshot-retries \
    && apt-get -o Acquire::Check-Valid-Until=false update \
    # Acquire::Retries does not retry a TLS connection reset (#348); a failed fetch aborts before dpkg runs.
    && for attempt in 1 2 3 4 5; do \
         apt-get install -y --no-install-recommends \
           ca-certificates curl gnupg git make ripgrep \
           jq python3-pip libstdc++6 libatomic1 \
           brotli gzip \
         && break; \
         [ "$attempt" -lt 5 ] || exit 1; sleep $((attempt * 15)); \
       done \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/docker.tgz \
        "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_STATIC_VERSION}.tgz" \
    && echo "${DOCKER_STATIC_SHA256}  /tmp/docker.tgz" | sha256sum --check - \
    && tar -xzf /tmp/docker.tgz -C /tmp docker/docker \
    && install /tmp/docker/docker /usr/local/bin/docker \
    && rm -rf /tmp/docker /tmp/docker.tgz \
    && mkdir -p /usr/local/lib/docker/cli-plugins \
    && curl -fsSL --retry 7 --retry-all-errors -o /usr/local/lib/docker/cli-plugins/docker-buildx \
        "https://github.com/docker/buildx/releases/download/v${DOCKER_BUILDX_VERSION}/buildx-v${DOCKER_BUILDX_VERSION}.linux-amd64" \
    && echo "${DOCKER_BUILDX_SHA256}  /usr/local/lib/docker/cli-plugins/docker-buildx" | sha256sum --check - \
    && chmod +x /usr/local/lib/docker/cli-plugins/docker-buildx

COPY requirements.lock /tmp/requirements.lock
RUN pip3 install --break-system-packages --require-hashes -r /tmp/requirements.lock \
    && rm /tmp/requirements.lock

COPY package.json package-lock.json /opt/markdownlint/
# braces (GHSA-vfj7-8cjw-p6xm, #266) and picomatch (GHSA-fc5q-39xc-vjpf) have no fixed release; drop each shim once one ships.
COPY shims /opt/markdownlint/shims
RUN node --test /opt/markdownlint/shims/braces/index.test.js /opt/markdownlint/shims/picomatch/index.test.js \
    && npm ci --omit=dev --ignore-scripts --prefix /opt/markdownlint \
    && ln -s /opt/markdownlint/node_modules/.bin/markdownlint-cli2 /usr/local/bin/markdownlint-cli2

# Install vale (prose linter for the docs site)
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/vale.tar.gz \
        "https://github.com/errata-ai/vale/releases/download/v${VALE_VERSION}/vale_${VALE_VERSION}_Linux_64-bit.tar.gz" \
    && echo "${VALE_SHA256}  /tmp/vale.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/vale.tar.gz -C /usr/local/bin vale \
    && rm /tmp/vale.tar.gz

# Install kind
RUN curl -fsSL --retry 7 --retry-all-errors -o /usr/local/bin/kind \
    "https://kind.sigs.k8s.io/dl/v${KIND_VERSION}/kind-linux-amd64" \
    && echo "${KIND_SHA256}  /usr/local/bin/kind" | sha256sum --check - \
    && chmod +x /usr/local/bin/kind

# Install kubectl
RUN curl -fsSL --retry 7 --retry-all-errors -o /usr/local/bin/kubectl \
    "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && echo "${KUBECTL_SHA256}  /usr/local/bin/kubectl" | sha256sum --check - \
    && chmod +x /usr/local/bin/kubectl

# Install Helm
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/helm.tar.gz "https://get.helm.sh/helm-v${HELM_VERSION}-linux-amd64.tar.gz" \
    && echo "${HELM_SHA256}  /tmp/helm.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/helm.tar.gz -C /usr/local/bin --strip-components=1 linux-amd64/helm \
    && rm /tmp/helm.tar.gz
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/helm-min.tar.gz "https://get.helm.sh/helm-v${HELM_MIN_VERSION}-linux-amd64.tar.gz" \
    && echo "${HELM_MIN_SHA256}  /tmp/helm-min.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/helm-min.tar.gz -C /tmp --strip-components=1 linux-amd64/helm \
    && mv /tmp/helm /usr/local/bin/helm-min \
    && rm /tmp/helm-min.tar.gz

# Install yq
RUN curl -fsSL --retry 7 --retry-all-errors -o /usr/local/bin/yq \
    "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_amd64" \
    && echo "${YQ_SHA256}  /usr/local/bin/yq" | sha256sum --check - \
    && chmod +x /usr/local/bin/yq

COPY tools/go.mod tools/go.sum /opt/ci-tools/
RUN cd /opt/ci-tools \
    && go mod verify \
    && go build -mod=readonly -trimpath -o /usr/local/bin/gocover-cobertura github.com/boumenot/gocover-cobertura \
    && go build -mod=readonly -trimpath -o /usr/local/bin/crane github.com/google/go-containerregistry/cmd/crane \
    && go build -mod=readonly -trimpath -o /usr/local/bin/oras oras.land/oras/cmd/oras

# Install chart-testing (ct) with configuration files
RUN mkdir -p /etc/ct \
    && curl -fsSL --retry 7 --retry-all-errors -o /tmp/chart-testing.tar.gz \
        "https://github.com/helm/chart-testing/releases/download/v${CT_VERSION}/chart-testing_${CT_VERSION}_linux_amd64.tar.gz" \
    && echo "${CT_SHA256}  /tmp/chart-testing.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/chart-testing.tar.gz -C /tmp \
    && mv /tmp/ct /usr/local/bin/ct \
    && mv /tmp/etc/* /etc/ct/ \
    && rm -rf /tmp/etc /tmp/chart-testing.tar.gz

# Helm 3 rejects the release archive's Helm 4-only platformHooks field.
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/helm-unittest.tar.gz \
        "https://github.com/helm-unittest/helm-unittest/releases/download/v${HELM_UNITTEST_VERSION}/helm-unittest-linux-amd64-${HELM_UNITTEST_VERSION}.tgz" \
    && echo "${HELM_UNITTEST_SHA256}  /tmp/helm-unittest.tar.gz" | sha256sum --check - \
    && plugin_dir="$(helm env HELM_PLUGINS)/helm-unittest" \
    && mkdir -p "${plugin_dir}" \
    && tar -xzf /tmp/helm-unittest.tar.gz -C "${plugin_dir}" \
    && yq -i 'del(.platformHooks)' "${plugin_dir}/plugin.yaml" \
    && rm /tmp/helm-unittest.tar.gz

# Install kubeconform
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/kubeconform.tar.gz \
        "https://github.com/yannh/kubeconform/releases/download/v${KUBECONFORM_VERSION}/kubeconform-linux-amd64.tar.gz" \
    && echo "${KUBECONFORM_SHA256}  /tmp/kubeconform.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/kubeconform.tar.gz -C /usr/local/bin kubeconform \
    && rm /tmp/kubeconform.tar.gz

# Install goreleaser
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/goreleaser.tar.gz \
        "https://github.com/goreleaser/goreleaser/releases/download/v${GORELEASER_VERSION}/goreleaser_Linux_x86_64.tar.gz" \
    && echo "${GORELEASER_SHA256}  /tmp/goreleaser.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/goreleaser.tar.gz -C /usr/local/bin goreleaser \
    && rm /tmp/goreleaser.tar.gz

# Install cosign
RUN curl -fsSL --retry 7 --retry-all-errors -o /usr/local/bin/cosign \
    "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-amd64" \
    && echo "${COSIGN_SHA256}  /usr/local/bin/cosign" | sha256sum --check - \
    && chmod +x /usr/local/bin/cosign

# Install syft for SBOM generation
RUN curl -fsSL --retry 7 --retry-all-errors -o /tmp/syft.tar.gz \
        "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/syft_${SYFT_VERSION}_linux_amd64.tar.gz" \
    && echo "${SYFT_SHA256}  /tmp/syft.tar.gz" | sha256sum --check - \
    && tar -xzf /tmp/syft.tar.gz -C /usr/local/bin syft \
    && rm /tmp/syft.tar.gz

# Verify installations
RUN go version && haproxy -v && docker --version && docker buildx version && kind version && kubectl version --client && helm version && helm-min version && yq --version && jq --version && rg --version && gocover-cobertura -h 2>&1 | head -1 && yamllint --version && markdownlint-cli2 --help | head -1 && ct version && helm unittest --help >/dev/null && kubeconform -v && goreleaser --version && cosign version && syft version && crane version && oras version && vale --version
