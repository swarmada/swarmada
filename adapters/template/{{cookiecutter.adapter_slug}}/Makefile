# {{cookiecutter.adapter_name}} — developer targets.
PYTHON ?= python3
# A checkout of the Swarmada repository: the conformance harness and the fleet_adapter.v1
# Python stubs come from it.
SWARMADA ?= ../swarmada
PORT ?= 9090
REPORT ?= conformance-report.json

SWARMADA_ABS = $(abspath $(SWARMADA))

.PHONY: test conformance profile
test:            ## Run the safety-wiring tests
	$(PYTHON) -m pytest tests/ -v

conformance:     ## Run the Swarmada conformance harness (C0-C19) against this adapter
	@test -d "$(SWARMADA_ABS)/adapters/conformance" || { \
	  echo "SWARMADA=$(SWARMADA) is not a Swarmada checkout; pass SWARMADA=<path>"; exit 2; }
	cd "$(SWARMADA_ABS)" && $(PYTHON) -m adapters.conformance --stub-path proto \
	  --adapter-name {{cookiecutter.adapter_slug}} --port $(PORT) --json "$(abspath $(REPORT))" \
	  --adapter-cmd 'PYTHONPATH=$(SWARMADA_ABS)/proto:$$PYTHONPATH $(PYTHON) -m {{cookiecutter.python_package}}.adapter --endpoint localhost:{port}'

profile:         ## Group the last conformance report into profiles and print the verdict
	cd "$(SWARMADA_ABS)" && $(PYTHON) -m conformance fleet-adapter --adapter-report "$(abspath $(REPORT))"
