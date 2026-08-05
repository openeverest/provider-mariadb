HELM ?= helm

# values to override
IMAGE_PREFIX ?= ghcr.io/openeverest
PROVIDER_IMAGE_NAME ?= provider-mariadb
VERSION ?= 0.1.0
CHART_VERSION ?= $(VERSION)

.PHONY: prepare-chart
prepare-chart:
	yq eval -i '.version = "$(CHART_VERSION)"' Chart.yaml
	yq eval -i '.appVersion = "$(VERSION)"' Chart.yaml
	yq eval -i '.image.repository = "$(IMAGE_PREFIX)/$(PROVIDER_IMAGE_NAME)"' values.yaml
	yq eval -i '.image.tag = "$(VERSION)"' values.yaml
	$(MAKE) deps

.PHONY: release
release: prepare-chart

.PHONY: release-dev
release-dev: PROVIDER_IMAGE_NAME := provider-mariadb-dev
release-dev: prepare-chart

.PHONY: deps
deps:
	$(HELM) version
	# The subcharts live in the mariadb-operator Helm repo; register it so
	# `helm dependency build` can resolve them.
	$(HELM) repo add mariadb-operator https://mariadb-operator.github.io/mariadb-operator --force-update
	$(HELM) dependency build .
