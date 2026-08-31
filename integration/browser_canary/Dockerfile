FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

RUN apt-get update \
 && apt-get install --no-install-recommends --assume-yes strace=6.1-0.1 \
 && rm -rf /var/lib/apt/lists/* \
 && install -d -m 0700 /canary/scratch \
 && install -d -m 0777 /canary/evidence \
 && install -d -m 0755 /canary/decoys

COPY .tmp/browser-canary/native /usr/local/bin/opencode
COPY .tmp/browser-canary/browser-canary.test /canary/browser-canary.test
COPY integration/browser_canary/decoy.sh /canary/decoys/open
COPY integration/browser_canary/entrypoint.sh /canary/entrypoint.sh

RUN chmod 0755 /usr/local/bin/opencode /canary/browser-canary.test /canary/decoys/open /canary/entrypoint.sh \
 && for name in xdg-open x-www-browser www-browser sensible-browser gio firefox google-chrome google-chrome-stable chromium chromium-browser; do cp /canary/decoys/open "/canary/decoys/$name"; done \
 && /canary/entrypoint.sh --verify-native

WORKDIR /canary
ENTRYPOINT ["/canary/entrypoint.sh"]
