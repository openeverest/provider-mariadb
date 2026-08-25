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

// Package mariadbphysical contains the schema-bearing Go types for the
// "mariadb-physical" BackupClass. Each struct here is converted to an OpenAPI
// v3 schema by `provider-sdk generate` and inlined into the generated
// BackupClass manifest.
//
// +k8s:openapi-gen=true
package mariadbphysical

// MariadbPhysicalBackupParameters describes the parameters accepted by Backup
// CRs that target this class (spec.parameters). They map onto the corresponding
// fields of the mariadb-operator PhysicalBackup CR.
type MariadbPhysicalBackupParameters struct {
	// Compression selects the algorithm mariadb-backup uses to compress the
	// snapshot before uploading it. Defaults to none.
	// +kubebuilder:validation:Enum=none;bzip2;gzip
	// +optional
	Compression string `json:"compression,omitempty"`
	// Target selects the Pod the physical backup is taken from. "Replica" only
	// uses a ready replica; "PreferReplica" falls back to the primary when no
	// ready replica is available. Defaults to "PreferReplica" so single-node
	// instances still take the backup from the primary.
	// +kubebuilder:validation:Enum=Replica;PreferReplica
	// +optional
	Target string `json:"target,omitempty"`
}

// MariadbPhysicalRestoreParameters describes the parameters accepted by Restore
// CRs that target this class (spec.parameters). Physical restores are performed
// by seeding a new Instance via bootstrapFrom and expose no restore-time
// options; the struct is intentionally empty and ships an empty schema until
// requirements crystallize.
type MariadbPhysicalRestoreParameters struct{}
