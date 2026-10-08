TARGETS := all build run test fmt lint clean linux-build fault-build docker-build compose-up compose-down compose-clean integration

.PHONY: $(TARGETS)

$(TARGETS):
	$(MAKE) -C agent $@
