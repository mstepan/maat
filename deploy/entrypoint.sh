#!/bin/sh
set -eu

# PostgreSQL starts only through the agent's authority/recovery checks.
# Restarting this supervisor never invokes the image's database entrypoint.
if [ "$(id -u)" != 0 ]; then
    echo "maat supervisor requires root for socket and secret setup" >&2
    exit 1
fi
socket=/var/run/docker.sock
if [ ! -S "$socket" ]; then
    echo "Docker socket is unavailable" >&2
    exit 1
fi
socket_gid=$(stat -c '%g' "$socket")
if ! getent group "$socket_gid" >/dev/null; then
    groupadd --gid "$socket_gid" maat-docker
fi
usermod --append --groups "$socket_gid" postgres

install -d -m 0700 -o postgres -g postgres /var/lib/maat /var/lib/maat/postgres /var/lib/maat/control
install -d -m 0750 -o root -g postgres /run/maat
for name in postgres-password replication-password; do
    if [ ! -s "/run/secrets/$name" ]; then
        echo "Required mounted credential file is empty or missing" >&2
        exit 1
    fi
    install -m 0600 -o postgres -g postgres "/run/secrets/$name" "/run/maat/$name"
done

agent_pid=
shutdown() {
    trap '' TERM INT
    if [ -n "$agent_pid" ]; then
        kill -TERM "$agent_pid" 2>/dev/null || true
        wait "$agent_pid" 2>/dev/null || true
    fi
    if [ -f /var/lib/maat/postgres/data/postmaster.pid ]; then
        gosu postgres pg_ctl -D /var/lib/maat/postgres/data -m fast -w -t 15 stop || true
    fi
    exit 0
}
trap shutdown TERM INT

while :; do
    gosu postgres /usr/local/bin/maat run --config /etc/maat/config.json &
    agent_pid=$!
    status=0
    wait "$agent_pid" || status=$?
    agent_pid=
    echo "maat agent exited (status $status); restarting without changing PostgreSQL role" >&2
    sleep 1 &
    wait $! || true
done
