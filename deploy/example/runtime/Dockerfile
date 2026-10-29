FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8 LC_ALL=C.UTF-8
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates bash git curl file unzip jq ripgrep python3 \
      python3-pandas python3-openpyxl python3-xlsxwriter python3-matplotlib \
      python3-docx python3-pypdf python3-pil pandoc poppler-utils \
      nodejs npm fonts-dejavu-core fonts-liberation2 \
    && rm -rf /var/lib/apt/lists/*
COPY package.json package-lock.json /opt/opencode/
RUN cd /opt/opencode && npm ci --ignore-scripts --omit=dev --no-audit --no-fund \
    && ln -s /opt/opencode/node_modules/@opencode/cli-linux-x64/bin/opencode /usr/local/bin/opencode \
    && opencode --version
RUN useradd -u 1000 -U -m -d /home/user -s /bin/bash user \
    && mkdir -p /workspace && chown user:user /workspace
ENV HOME=/home/user \
    XDG_CONFIG_HOME=/home/user/.config \
    XDG_DATA_HOME=/home/user/.local/share \
    XDG_STATE_HOME=/home/user/.local/state \
    XDG_CACHE_HOME=/home/user/.cache \
    OPENCODE_DISABLE_MODELS_FETCH=1 \
    OPENCODE_DISABLE_AUTOUPDATE=1
USER 1000:1000
WORKDIR /workspace
EXPOSE 4096
ENTRYPOINT ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"]
