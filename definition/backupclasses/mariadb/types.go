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

// Package mariadbbackup contains the schema-bearing Go types for the "mariadb"
// BackupClass. Each struct here is converted to an OpenAPI v3 schema by
// `provider-sdk generate` and inlined into the generated BackupClass manifest.
//
// +k8s:openapi-gen=true
package mariadbbackup

// MariadbBackupParameters describes the parameters accepted by Backup CRs and
// InstanceBackupSchedules that target this class (spec.parameters). The Type
// field selects between logical (mariadb-dump) and physical (mariadb-backup)
// backups, and the provider creates the corresponding mariadb-operator CR
// (Backup or PhysicalBackup). Fields that only apply to one type are ignored
// for the other.
type MariadbBackupParameters struct {
	// Type selects the backup strategy: "logical" (mariadb-dump, a SQL dump) or
	// "physical" (mariadb-backup, a data directory snapshot). Defaults to
	// "logical" when empty.
	// +kubebuilder:validation:Enum=logical;physical
	// +optional
	Type string `json:"type,omitempty"`
	// Compression selects the algorithm used to compress the backup. Applies to
	// both types. Defaults to none.
	// +kubebuilder:validation:Enum=none;bzip2;gzip
	// +optional
	Compression string `json:"compression,omitempty"`
	// Databases restricts a logical backup to the named databases. When empty,
	// all databases are backed up. Logical backups only.
	// +optional
	Databases []string `json:"databases,omitempty"`
	// IgnoreGlobalPriv excludes the mysql.global_priv table from a logical dump.
	// When unset the operator defaults it to true for Galera and false
	// otherwise. Logical backups only.
	// +optional
	IgnoreGlobalPriv *bool `json:"ignoreGlobalPriv,omitempty"`
	// Target selects the Pod a physical backup is taken from: "Replica" only
	// uses a ready replica; "PreferReplica" falls back to the primary when no
	// ready replica is available. Defaults to "PreferReplica" so single-node
	// instances still take the backup from the primary. Physical backups only.
	// +kubebuilder:validation:Enum=Replica;PreferReplica
	// +optional
	Target string `json:"target,omitempty"`
}

// MariadbRestoreParameters describes the parameters accepted by Restore CRs
// that target this class (spec.parameters). Restores expose no restore-time
// options yet; the struct is intentionally empty and ships an empty schema
// until requirements crystallize.
type MariadbRestoreParameters struct{}
