# A tiny local service for the port-publish smoke. Serves a
# distinctive body on :80 so the host-side curl proves ingress reached
# *this* container. nginx:alpine (not nginx:latest) keeps the rootfs
# small — the smoke runs several sandboxes concurrently and, on a
# filesystem without reflink, each is a full byte-copy of the rootfs;
# a ~400MB base exhausts a tmpfs /tmp. Same nginx, same :80, ~10x smaller.
FROM nginx:alpine
RUN printf 'CRUCIBLE-TESTAPP-OK\n' > /usr/share/nginx/html/index.html
