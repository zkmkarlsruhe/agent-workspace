# syntax=docker/dockerfile:1

# Use the registry directly; cross-host redirects can lose registry authentication.
FROM ghcr.io/linuxserver/freecad@sha256:e84add50316b74fdc4e89c7da967a6eb0114257b710080864e3ba2c5adfe72c6

RUN curl --fail --location --output /tmp/bridge.tar.gz \
    https://codeload.github.com/CREATeNG/freecad-mcp-bridge/tar.gz/4cb35facb1ed117bf4e2a2ed360d8d34e80a87f5 \
    && echo "9a418f813f68e0613b93a291963035669badb97af8baab733fcd9a263e5b17ec  /tmp/bridge.tar.gz" | sha256sum -c - \
    && mkdir -p /opt/freecad-bridge /opt/workspace-freecad /projects \
    && tar -xzf /tmp/bridge.tar.gz --strip-components=1 -C /opt/freecad-bridge \
    && rm /tmp/bridge.tar.gz \
    && chown -R 1000:1000 /config /projects

COPY start-desktop recovery.py start-freecad start.FCMacro workspace_cad.py mcp_stdio.py probe.py control.py configure.py tools.json INSTRUCTIONS.md defaults.cfg openbox.xml /opt/workspace-freecad/
RUN chmod 755 /opt/workspace-freecad/start-freecad /opt/workspace-freecad/start-desktop
ENV HOME=/config PYTHONDONTWRITEBYTECODE=1 PIXELFLUX_WAYLAND=false \
    SELKIES_ALLOWED_ORIGINS=""
WORKDIR /config
USER 1000:1000
ENTRYPOINT ["/opt/workspace-freecad/start-desktop", "--session", "/opt/workspace-freecad/start-freecad", "--public", "--port", "3000", "--web-root", "/usr/share/selkies/selkies-dashboard", "--wayland", "false", "--audio-enabled", "false", "--microphone-enabled", "false", "--gamepad-enabled", "false", "--webcam-enabled", "false", "--command-enabled", "false", "--file-transfers", "", "--enable-sharing", "false", "--enable-clipboard", "false", "--ui-sidebar-show-apps", "false"]
HEALTHCHECK --interval=30s --timeout=20s --start-period=90s --retries=3 \
    CMD python3 /opt/workspace-freecad/probe.py
