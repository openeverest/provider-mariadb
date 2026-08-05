// Package components contains parameter types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
//
// +k8s:openapi-gen=true
package components

// MariadbParameters defines optional per-component parameters for MariaDB engine nodes.
type MariadbParameters struct {
	// MyCnf is additional MariaDB configuration to be appended to the default my.cnf.
	// Content is passed verbatim to the operator's spec.myCnf field.
	// +optional
	MyCnf string `json:"myCnf,omitempty"`
}

// MonitoringParameters defines parameters for the monitoring component.
type MonitoringParameters struct {
	// MonitoringConfigName is the name of the MonitoringConfig resource to use.
	// If not specified, monitoring is not configured.
	// +optional
	MonitoringConfigName *string `json:"monitoringConfigName,omitempty"`
}
