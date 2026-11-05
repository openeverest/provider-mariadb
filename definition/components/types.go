// Package components contains parameter types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
//
// +k8s:openapi-gen=true
package components

import "fmt"

// FlexBool is a boolean that also decodes from the JSON strings "true"/"false".
// The OpenEverest UI renders boolean toggles as string-valued `select` widgets,
// so a value can arrive as either a JSON bool (API/preset) or a quoted string (UI).
//
// +kubebuilder:validation:Type=""
// +kubebuilder:validation:Enum=true;false;"true";"false"
type FlexBool bool

// UnmarshalJSON accepts a JSON bool or the quoted strings "true"/"false".
func (b *FlexBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", `"true"`:
		*b = true
	case "false", `"false"`, `""`, "null":
		*b = false
	default:
		return fmt.Errorf("invalid boolean value: %s", string(data))
	}
	return nil
}

// MariadbParameters defines optional per-component parameters for MariaDB engine nodes.
type MariadbParameters struct {
	// Configuration is additional MariaDB configuration appended to the default my.cnf.
	// `configuration` is the conventional engine-config property name across providers;
	// the content is passed verbatim to the operator's spec.myCnf field.
	// +optional
	Configuration string `json:"configuration,omitempty"`
	// TLS configures transport encryption for MariaDB. TLS is enabled by
	// default when this block or its enabled field is omitted.
	// +optional
	TLS *TLSParameters `json:"tls,omitempty"`
}

// TLSParameters configures MariaDB transport encryption. Pointer booleans are
// used so an omitted value can be distinguished from an explicit false value.
type TLSParameters struct {
	// Enabled controls certificate issuance and TLS support. Defaults to true.
	// +optional
	Enabled *FlexBool `json:"enabled,omitempty"`
	// Required rejects unencrypted client connections. Defaults to false so
	// existing username/password clients can migrate to TLS without disruption.
	// +optional
	Required *FlexBool `json:"required,omitempty"`
	// GaleraSSTEnabled encrypts Galera state snapshot transfers. It defaults to
	// true for Galera instances and is not valid for standalone instances.
	// +optional
	GaleraSSTEnabled *FlexBool `json:"galeraSSTEnabled,omitempty"`
}

// MaxScaleParameters defines parameters for the proxy component.
//
// The proxy component deploys MariaDB MaxScale in front of a Galera or
// replication cluster. MaxScale routes writes to the primary and balances reads
// across replicas; primary failover stays with the mariadb-operator.
// Client connection details point at MaxScale while it is enabled.
type MaxScaleParameters struct {
	// Enabled deploys MaxScale in front of the cluster. Only supported on the
	// galera and replication topologies. Defaults to false.
	// +optional
	Enabled FlexBool `json:"enabled,omitempty"`
}
