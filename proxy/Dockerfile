FROM python:3.13-slim

WORKDIR /app

RUN pip install --no-cache-dir --upgrade pip
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY aio_proxy.py aio_login.py entrypoint.sh /app/
# COPY preserves the source files' modes, which can be 0600 if the working
# tree's umask was tight. Force a- world-readable on the scripts and x on
# the entrypoint so the container can run as an arbitrary non-root UID
# (e.g. --user $(id -u):$(id -g) from a host).
RUN chmod 0755 /app/entrypoint.sh && chmod 0644 /app/aio_proxy.py /app/aio_login.py

# /config is where the host-side state directory should be mounted.
# Both the proxy (which reads current.json + writes oidc.json on refresh) and
# the login script (which writes oidc.json on first auth) point at it via
# AIO_STATE_DIR.
ENV AIO_STATE_DIR=/config
VOLUME ["/config"]

# Default to running the MCP server. Override the entrypoint arg to "login"
# to run the interactive device-flow once.
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["serve"]
