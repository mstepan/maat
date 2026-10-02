#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$root/.secrets"
chmod 0700 "$root/.secrets"
for name in postgres-password replication-password; do
    path="$root/.secrets/$name"
    if [ -L "$path" ]; then
        echo "Refusing a symlink credential file" >&2
        exit 1
    fi
    if [ ! -e "$path" ]; then
        # Noclobber preserves credentials if preparation runs concurrently.
        (set -C; openssl rand -hex 32 > "$path")
    fi
    if [ ! -f "$path" ] || [ ! -s "$path" ]; then
        echo "Credential file is empty or not a regular file" >&2
        exit 1
    fi
    chmod 0600 "$path"
done
echo "Development credentials are ready in .secrets (values not printed)."
