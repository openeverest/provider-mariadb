// Package standalone contains parameter types for the standalone MariaDB topology.
//
// +k8s:openapi-gen=true
package standalone

// StandaloneTopologyParameters holds topology-level parameters for the standalone topology.
// Currently empty — the topology has no parameters beyond the standard component spec.
// Add fields here when topology-level configuration is needed.
type StandaloneTopologyParameters struct{}
