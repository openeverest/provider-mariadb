// Package common defines shared constants used across the provider.
package common

const (
	// ProviderName is the canonical name registered for this provider's Provider CR.
	ProviderName = "mariadb"

	// ComponentEngine is the logical component name for the MariaDB engine.
	ComponentEngine = "engine"

	// ComponentMonitoring is the logical component name for metrics/monitoring.
	ComponentMonitoring = "monitoring"

	// ComponentTypeMariaDB is the component type name that maps to the mariadb container.
	ComponentTypeMariaDB = "mariadb"
)
