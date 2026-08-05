// Package definition contains shared types used across the provider definition.
//
// +k8s:openapi-gen=true
package definition

// TopologyType represents a supported MariaDB deployment topology.
type TopologyType string

const (
	// TopologyTypeStandalone is a single MariaDB node (no HA).
	TopologyTypeStandalone TopologyType = "standalone"
	// TopologyTypeGalera is a multi-master Galera cluster (production HA).
	// Added in Phase 3.
	TopologyTypeGalera TopologyType = "galera"
	// TopologyTypeReplication is async primary/replica replication.
	// Added in Phase 4 (operator marks this feature alpha).
	TopologyTypeReplication TopologyType = "replication"
)
