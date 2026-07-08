# Tiny service for the logs smoke. Prints a distinctive startup line, then
# a periodic tick, so the smoke can assert durable capture (the startup
# line survives a crash/delete) and --follow (new ticks stream in).
FROM busybox:latest
CMD ["sh", "-c", "echo CRUCIBLE-LOG-HELLO; while true; do echo CRUCIBLE-TICK; sleep 2; done"]
