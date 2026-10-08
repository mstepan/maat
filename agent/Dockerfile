FROM postgres:18-bookworm@sha256:3725f4e2499eef5134592b3b4ab79a543ed7f8e533b05b5b637af926630f6650

COPY bin/maat-linux /usr/local/bin/maat
COPY deploy/entrypoint.sh /usr/local/bin/maat-entrypoint
RUN chmod 0755 /usr/local/bin/maat /usr/local/bin/maat-entrypoint
ENTRYPOINT ["/usr/local/bin/maat-entrypoint"]
