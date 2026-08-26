// Package replication contains parameter types for the async replication MariaDB topology.
//
// +k8s:openapi-gen=true
package replication

// ReplicationTopologyParameters holds topology-level parameters for the replication topology.
// Currently empty — the topology has no parameters beyond the standard component
// spec. Add fields here when topology-level configuration is needed.
type ReplicationTopologyParameters struct{}
