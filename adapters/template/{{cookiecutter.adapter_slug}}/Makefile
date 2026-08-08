# {{cookiecutter.adapter_name}} — developer targets.
PYTHON ?= python3

.PHONY: test conformance
test:            ## Run the safety-wiring tests
	pytest tests/ -v

conformance:     ## Drive this adapter against the Swarmada conformance harness.
	@echo "Point the published harness at:  $(PYTHON) -m {{cookiecutter.python_package}}.adapter --endpoint localhost:{port}"
	@echo "See the Swarmada adapters/conformance/README.md for running the harness."
