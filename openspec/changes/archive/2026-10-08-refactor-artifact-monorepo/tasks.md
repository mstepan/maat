## 1. Move the implemented artifact

- [x] 1.1 Move all tracked root Go sources/tests, `go.mod`, `go.sum`, and `internal/` into `agent/`; retain module path, dependency versions, and controller logic.
- [x] 1.2 Move the existing Makefile, Dockerfile, `.dockerignore`, `compose.yaml`, and complete `deploy/` directory into `agent/`; inspect callers, fixture paths, Docker COPY/mounts, runner root discovery, and binary defaults for any required path corrections.
- [x] 1.3 Add a root Makefile that forwards all existing targets, preserves the default `all` target, propagates overrides/failures, and generates agent assets only under `agent/bin/`.

## 2. Establish artifact documentation

- [x] 2.1 Move detailed agent operations into `agent/README.md`; write a root overview with artifact status, root commands, new output locations, and fresh-lab expectations.
- [x] 2.2 Add README-only `dashboard/`, `k8s-controller/`, and `helm-deployment/` placeholders; record the future TUI, `PostgresqlCluster` kind, and Helm purpose without implementation scaffolding.
- [x] 2.3 Update the CI Go module/cache paths and active source/command links in `AGENTS.md`, `docs/`, and `openspec/config.yaml`; preserve historical validation dates/results and archived proposal history.
- [x] 2.4 Update ignore rules for agent-generated assets and credentials while protecting legacy generated locations; verify ignored paths without displaying secrets.

## 3. Verify development entry points

- [x] 3.1 Run root `make fmt`, `make test`, `make lint`, and `make build`; verify `make -C agent build` and `make -C agent test` also work independently. Report any unavailable checks and reasons.
- [x] 3.2 Verify root target forwarding and `MAAT_DOCKER_ARCH` override with Make dry runs; run root `make run` and confirm existing usage plus nonzero failure propagation. Check `clean` resolution without removing retained investigation assets unnecessarily.
- [x] 3.3 Check moved shell scripts with `sh -n` and Compose with `docker compose -f agent/compose.yaml config --quiet`; confirm build context, configuration/credential paths, and container-internal paths resolve correctly. Verify root/agent README links and current commands against the new Makefiles.

## 4. Verify deployment and record results

- [x] 4.1 Run root `make integration` in a fresh isolated project; inspect verified fencing, one replacement primary, replica following, old-primary rejoin, project isolation, and retained default cleanup assets.
- [x] 4.2 Exercise moved ordinary/fault-runner paths: run `python3 agent/deploy/run_integration.py --scenario authorized-crash`; build with root `make fault-build`; run `python3 agent/deploy/concurrent_transition.py --cleanup --timeout 150` and one crash case through `python3 agent/deploy/transition_crashes.py --binary agent/bin/maat-faults-linux --point after-authorize --mode crash --remove-test-volumes`. Explicit removal applies only to these newly created disposable fault projects. Report unavailable/failed checks rather than claiming they passed.
- [x] 4.3 Record fresh results and remaining gaps in `docs/validation.md`; check the final diff for runtime/configuration changes, accidental secrets, unused scaffolding, and stale active paths. Run strict OpenSpec validation before handoff. Preserve development-only and asynchronous transaction-loss limitations.
