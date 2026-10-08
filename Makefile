TARGETS := all build run test fmt lint clean linux-build fault-build docker-build compose-up compose-down compose-clean integration

.PHONY: $(TARGETS)

$(TARGETS):
	$(MAKE) -C agent $@

DASHBOARD_TARGETS := dashboard-fmt dashboard-test dashboard-lint dashboard-build dashboard-run dashboard-clean
.PHONY: $(DASHBOARD_TARGETS)

$(DASHBOARD_TARGETS):
	$(MAKE) -C dashboard $(patsubst dashboard-%,%,$@)
