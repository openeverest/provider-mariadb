// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mariadbdump contains the schema-bearing Go types for the
// "mariadb-dump" BackupClass. Each struct here is converted to an OpenAPI v3
// schema by `provider-sdk generate` and inlined into the generated BackupClass
// manifest.
//
// +k8s:openapi-gen=true
package mariadbdump

// MariadbDumpBackupParameters describes the parameters accepted by Backup CRs
// that target this class (spec.parameters). They map onto the corresponding
// fields of the mariadb-operator Backup CR.
type MariadbDumpBackupParameters struct {
	// Compression selects the algorithm mariadb-operator uses to compress the
	// dump before uploading it. Defaults to none.
	// +kubebuilder:validation:Enum=none;bzip2;gzip
	Compression string `json:"compression,omitempty"`
	// Databases restricts the backup to the named logical databases. When
	// empty, all databases are backed up.
	// +optional
	Databases []string `json:"databases,omitempty"`
	// IgnoreGlobalPriv excludes the mysql.global_priv table from the dump.
	// When unset the operator defaults it to true for Galera instances and
	// false otherwise.
	// +optional
	IgnoreGlobalPriv *bool `json:"ignoreGlobalPriv,omitempty"`
}

// MariadbDumpRestoreParameters describes the parameters accepted by Restore
// CRs that target this class (spec.parameters). Logical restores expose no
// restore-time options yet; the struct is intentionally empty and ships an
// empty schema until requirements crystallize.
type MariadbDumpRestoreParameters struct{}
