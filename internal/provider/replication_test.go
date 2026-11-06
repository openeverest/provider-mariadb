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

package provider

import (
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestIsHATopology(t *testing.T) {
	tests := []struct {
		topology string
		want     bool
	}{
		{topology: "", want: false},
		{topology: "standalone", want: false},
		{topology: "galera", want: true},
		{topology: "replication", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.topology, func(t *testing.T) {
			if got := isHATopology(newTopologyContext(t, tt.topology, nil)); got != tt.want {
				t.Errorf("isHATopology(%q) = %v, want %v", tt.topology, got, tt.want)
			}
		})
	}
}

func TestValidateReferences_ReplicationTopologyImmutable(t *testing.T) {
	replicationMariaDB := func() *mariadbv1alpha1.MariaDB {
		return &mariadbv1alpha1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: mariadbv1alpha1.MariaDBSpec{
				Replicas:    3,
				Replication: &mariadbv1alpha1.Replication{Enabled: true},
			},
		}
	}
	tests := []struct {
		name     string
		topology string
		existing *mariadbv1alpha1.MariaDB
		wantErr  bool
	}{
		{name: "replication stays replication", topology: "replication", existing: replicationMariaDB(), wantErr: false},
		{name: "standalone -> replication rejected", topology: "replication", existing: existingMariaDB(false), wantErr: true},
		{name: "galera -> replication rejected", topology: "replication", existing: existingMariaDB(true), wantErr: true},
		{name: "replication -> galera rejected", topology: "galera", existing: replicationMariaDB(), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTopologyContext(t, tt.topology, ptr.To(int32(3)), versionCatalogProvider(), tt.existing)
			errs, err := New().ValidateReferences(c)
			if err != nil {
				t.Fatalf("ValidateReferences() error = %v", err)
			}
			if (len(errs) > 0) != tt.wantErr {
				t.Fatalf("ValidateReferences() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}
