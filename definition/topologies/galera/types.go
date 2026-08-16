// Package galera contains parameter types for the Galera MariaDB topology.
//
// +k8s:openapi-gen=true
package galera

// GaleraTopologyParameters holds topology-level parameters for the Galera topology.
// Currently empty — the topology has no parameters beyond the standard component
// spec. Add fields here when topology-level configuration is needed.
type GaleraTopologyParameters struct{}
