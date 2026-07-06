# syntax=docker/dockerfile:1
#
# Builds a crucible-compatible guest rootfs from a native language base
# image. The container is never *run* — scripts/build-profile.sh exports
# its filesystem and packs it into an ext4 image that Firecracker boots
# with init=/sbin/init (systemd). We only inject what a microVM guest
# needs on top of the native toolchain: an init, systemd-networkd (DHCP
# on eth0), the crucible agent, and crucible's static DNS config.
ARG BASE=debian:12-slim
FROM ${BASE}

# crucible guest agent — built by `make agent`, placed in the build
# context by scripts/build-profile.sh.
COPY crucible-agent /usr/local/bin/crucible-agent

# Init + networking userspace. ca-certificates is installed the native
# (distro) way so guest HTTPS clients — pip, npm, go, curl — trust the
# standard bundle with no host-copy hacks.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        systemd systemd-sysv iproute2 ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    chmod 0755 /usr/local/bin/crucible-agent

# systemd-networkd DHCPs eth0 at boot and re-runs on link bounce (the
# agent's POST /network/refresh does `ip link set eth0 down/up` so a fork
# re-DHCPs to its per-netns address). UseDNS=no keeps our static resolver.
COPY <<'EOF' /etc/systemd/network/20-crucible-eth0.network
[Match]
Name=eth0

[Network]
DHCP=ipv4
LinkLocalAddressing=no
IPv6AcceptRA=no

[DHCP]
UseDNS=no
EOF

# Static resolver → the daemon's DNS anycast address; the daemon's DNS
# proxy enforces the sandbox's allowlist before forwarding upstream.
COPY <<'EOF' /etc/resolv.conf
# Managed by crucible's profile build.
nameserver 10.20.255.254
options edns0
EOF

# The agent runs as a systemd service so its exec'd children (python,
# node, go, curl) inherit a real PID-1 supervisor and the CA env.
COPY <<'EOF' /etc/systemd/system/crucible-agent.service
[Unit]
Description=crucible guest agent (vsock exec endpoint)
After=basic.target
Wants=basic.target
ConditionPathExists=/usr/local/bin/crucible-agent

[Service]
Type=simple
ExecStart=/usr/local/bin/crucible-agent
Restart=on-failure
RestartSec=1
Environment=SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
Environment=SSL_CERT_DIR=/etc/ssl/certs
Environment=REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
Environment=CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
EOF

# Enable networkd + the agent (creates the multi-user.target.wants
# symlinks the exported rootfs boots with). systemctl enable manipulates
# symlinks on the filesystem, so it works at build time with no running
# systemd.
RUN set -eux; \
    systemctl enable systemd-networkd.service; \
    systemctl enable crucible-agent.service
