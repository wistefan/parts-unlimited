# Agent worker container image.
# Contains Claude Code CLI, git, common build tools, and pre-installed
# toolchains (Go, Java/Maven, kubectl, Helm).

FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    curl \
    wget \
    jq \
    ca-certificates \
    openssh-client \
    build-essential \
    python3 \
    python3-pip \
    sudo \
    docker.io \
    && rm -rf /var/lib/apt/lists/*

# Install Go
ARG GO_VERSION=1.24.2
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# Install OpenJDK 21 (Eclipse Temurin LTS) and Maven.
# Bookworm's apt repos only ship openjdk-17, so JDK 21 is installed from the
# Adoptium tarball. Maven from apt is JDK-agnostic and picks up JAVA_HOME.
ARG TEMURIN_VERSION=21.0.10
ARG TEMURIN_BUILD=7
RUN mkdir -p /opt/jdk \
    && curl -fsSL "https://github.com/adoptium/temurin21-binaries/releases/download/jdk-${TEMURIN_VERSION}%2B${TEMURIN_BUILD}/OpenJDK21U-jdk_x64_linux_hotspot_${TEMURIN_VERSION}_${TEMURIN_BUILD}.tar.gz" \
    | tar -C /opt/jdk --strip-components=1 -xz \
    && apt-get update && apt-get install -y --no-install-recommends \
        maven \
    && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/opt/jdk
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# Install kubectl
ARG KUBECTL_VERSION=v1.32.3
RUN curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl

# Install Helm
ARG HELM_VERSION=v3.17.3
RUN curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" \
    | tar -xz --strip-components=1 -C /usr/local/bin linux-amd64/helm

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Create non-root agent user with UID 1000 (matching the K8s pod securityContext).
# The node base image ships a 'node' user at UID 1000, so remove it first.
# Grant passwordless sudo so the agent can install project-specific toolchains
# (Java, Maven, Docker, Go, etc.) at runtime.  The container is ephemeral and
# network-isolated, so sudo inside it carries no meaningful security risk.
RUN userdel -r node 2>/dev/null || true \
    && useradd -m -s /bin/bash -u 1000 agent \
    && echo "agent ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/agent
USER agent
WORKDIR /home/agent

# Go workspace for the agent user
ENV GOPATH=/home/agent/go
ENV PATH="${GOPATH}/bin:${PATH}"

# Git configuration for the agent (identity is set at runtime via env vars)
RUN git config --global init.defaultBranch main \
    && git config --global advice.detachedHead false

# Copy bootstrap script, usage parser, and system prompt templates (one per mode)
COPY --chown=agent:agent bootstrap.sh /home/agent/bootstrap.sh
COPY --chown=agent:agent parse-usage.py /home/agent/parse-usage.py
COPY --chown=agent:agent export-claude-spend.py /home/agent/export-claude-spend.py
COPY --chown=agent:agent summarize-session.py /home/agent/summarize-session.py
COPY --chown=agent:agent system-prompt.md /home/agent/system-prompt.md
COPY --chown=agent:agent system-prompt-analysis.md /home/agent/system-prompt-analysis.md
COPY --chown=agent:agent system-prompt-plan.md /home/agent/system-prompt-plan.md
COPY --chown=agent:agent system-prompt-step.md /home/agent/system-prompt-step.md
COPY --chown=agent:agent system-prompt-fix.md /home/agent/system-prompt-fix.md

ENTRYPOINT ["/bin/bash", "/home/agent/bootstrap.sh"]
