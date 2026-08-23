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
	// NodeAffinity restricts engine pods to nodes whose labels match the given rules,
	// one rule per line: "<key> <operator> [<value>,<value>...]" where operator is
	// In, NotIn, Exists or DoesNotExist (In/NotIn require values; Exists/DoesNotExist
	// take none). All rules are combined (AND) into a single required node affinity term.
	// Mutually exclusive with spec.components.engine.affinity.nodeAffinity.
	// +optional
	NodeAffinity string `json:"nodeAffinity,omitempty"`
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

// MonitoringParameters defines parameters for the monitoring component.
//
// The monitoring component maps onto the mariadb-operator's spec.metrics: it
// deploys a mysqld-exporter sidecar and, via ServiceMonitor, integrates with a
// Prometheus Operator stack. Both are opt-in and default to off.
type MonitoringParameters struct {
	// Enabled turns on the MariaDB metrics exporter (mysqld-exporter). When
	// false or unset, no exporter is deployed and no Prometheus objects are
	// created. Defaults to false.
	// +optional
	Enabled FlexBool `json:"enabled,omitempty"`
	// ServiceMonitor controls the creation of a Prometheus Operator
	// ServiceMonitor object that scrapes the exporter. Creating it requires the
	// ServiceMonitor CRD (monitoring.coreos.com) to be installed in the cluster.
	//
	// Note: the underlying mariadb-operator currently couples exporter and
	// ServiceMonitor reconciliation, so a ServiceMonitor is always created while
	// metrics are enabled, regardless of ServiceMonitor.Enabled.
	// +optional
	ServiceMonitor ServiceMonitorParameters `json:"serviceMonitor,omitempty"`
}

// ServiceMonitorParameters configures the Prometheus ServiceMonitor object
// created for the exporter. All fields are optional; the mariadb-operator and
// Prometheus apply sensible defaults when they are unset.
type ServiceMonitorParameters struct {
	// Enabled controls whether a Prometheus ServiceMonitor is created for the
	// exporter. Defaults to false. It is automatically enabled while metrics are
	// enabled, since the mariadb-operator reconciles the two together.
	// +optional
	Enabled FlexBool `json:"enabled,omitempty"`
	// PrometheusRelease is the value of the `release` label added to the
	// ServiceMonitor so a Prometheus instance selecting on it picks the target up.
	// +optional
	PrometheusRelease string `json:"prometheusRelease,omitempty"`
	// JobLabel is the label to use as the Prometheus job name for the target.
	// +optional
	JobLabel string `json:"jobLabel,omitempty"`
	// Interval at which Prometheus scrapes the exporter (e.g. "10s").
	// +optional
	Interval string `json:"interval,omitempty"`
	// ScrapeTimeout is the timeout for a single scrape (e.g. "10s").
	// +optional
	ScrapeTimeout string `json:"scrapeTimeout,omitempty"`
}
