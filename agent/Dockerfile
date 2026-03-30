# Agent worker container image.
# Contains Claude Code CLI, git, and common build tools.
# Language-specific toolchains are installed by the agent at runtime.

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
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Create non-root agent user with UID 1000 (matching the K8s pod securityContext).
# The node base image ships a 'node' user at UID 1000, so remove it first.
RUN userdel -r node 2>/dev/null || true \
    && useradd -m -s /bin/bash -u 1000 agent
USER agent
WORKDIR /home/agent

# Git configuration for the agent (identity is set at runtime via env vars)
RUN git config --global init.defaultBranch main \
    && git config --global advice.detachedHead false

# Copy bootstrap script and system prompt template
COPY --chown=agent:agent bootstrap.sh /home/agent/bootstrap.sh
COPY --chown=agent:agent system-prompt.md /home/agent/system-prompt.md

ENTRYPOINT ["/bin/bash", "/home/agent/bootstrap.sh"]
