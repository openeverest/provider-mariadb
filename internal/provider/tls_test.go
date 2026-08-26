// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"errors"
	"testing"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-mariadb/internal/common"
)

func newTLSContext(t *testing.T, topology, params string, objs ...client.Object) *controller.Context {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add OpenEverest scheme: %v", err)
	}
	if err := mariadbv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add MariaDB scheme: %v", err)
	}

	engine := corev1alpha1.ComponentSpec{Name: common.ComponentEngine, Type: common.ComponentTypeMariaDB}
	if params != "" {
		engine.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
	if topology != "" {
		instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: topology}
	}

	objects := append([]client.Object{instance}, objs...)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return controller.NewContext(context.Background(), k8sClient, instance, common.ProviderName)
}

func TestResolveTLSSettings(t *testing.T) {
	tests := []struct {
		name     string
		topology string
		params   string
		want     tlsSettings
	}{
		{
			name: "standalone defaults to TLS enabled without enforcement",
			want: tlsSettings{Enabled: true},
		},
		{
			name:     "galera defaults to TLS and encrypted SST",
			topology: "galera",
			want:     tlsSettings{Enabled: true, GaleraSSTEnabled: true},
		},
		{
			name:     "replication defaults to TLS without SST",
			topology: "replication",
			want:     tlsSettings{Enabled: true},
		},
		{
			name:     "SST parameter is ignored off Galera",
			topology: "replication",
			params:   `{"tls":{"galeraSSTEnabled":true}}`,
			want:     tlsSettings{Enabled: true},
		},
		{
			name:   "TLS can be disabled explicitly",
			params: `{"tls":{"enabled":false}}`,
			want:   tlsSettings{},
		},
		{
			name:   "UI string booleans are decoded",
			params: `{"tls":{"enabled":"true","required":"true"}}`,
			want:   tlsSettings{Enabled: true, Required: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTLSSettings(newTLSContext(t, tt.topology, tt.params))
			if err != nil {
				t.Fatalf("resolveTLSSettings() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveTLSSettings() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateTLS(t *testing.T) {
	tests := []struct {
		name     string
		topology string
		params   string
		wantErr  bool
	}{
		{name: "defaults are valid"},
		{name: "required with TLS", params: `{"tls":{"required":true}}`},
		{name: "required while disabled", params: `{"tls":{"enabled":false,"required":true}}`, wantErr: true},
		{name: "SST encryption ignored on standalone", params: `{"tls":{"galeraSSTEnabled":true}}`},
		{name: "SST encryption ignored on replication", topology: "replication", params: `{"tls":{"galeraSSTEnabled":true}}`},
		{name: "SST encryption while disabled", topology: "galera", params: `{"tls":{"enabled":false,"galeraSSTEnabled":true}}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTLS(newTLSContext(t, tt.topology, tt.params))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTLS() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyTLSOverlayPreservesCertificateReferences(t *testing.T) {
	mdb := &mariadbv1alpha1.MariaDB{
		Spec: mariadbv1alpha1.MariaDBSpec{
			TLS: &mariadbv1alpha1.TLS{
				Enabled:                   false,
				ServerCASecretRef:         &mariadbv1alpha1.LocalObjectReference{Name: "custom-server-ca"},
				ClientCertSecretRef:       &mariadbv1alpha1.LocalObjectReference{Name: "custom-client-cert"},
				ServerCertAdditionalNames: []string{"db.example.com"},
			},
		},
	}
	desired := &mariadbv1alpha1.TLS{Enabled: true}
	applyTLSOverlay(mdb, desired)

	if !mdb.Spec.TLS.Enabled {
		t.Fatal("expected TLS to be enabled")
	}
	if mdb.Spec.TLS.ServerCASecretRef.Name != "custom-server-ca" ||
		mdb.Spec.TLS.ClientCertSecretRef.Name != "custom-client-cert" ||
		len(mdb.Spec.TLS.ServerCertAdditionalNames) != 1 {
		t.Fatalf("certificate configuration was not preserved: %+v", mdb.Spec.TLS)
	}
}

func TestBuildConnectionDetailsIncludesTLSCA(t *testing.T) {
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{userPasswordSecretKey: []byte("password")},
	}
	caBundle := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tlsCABundleSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{tlsCAKey: []byte("test-ca")},
	}

	details, err := buildConnectionDetails(newTLSContext(t, "", "", credentials, caBundle))
	if err != nil {
		t.Fatalf("buildConnectionDetails() error = %v", err)
	}
	if details.AdditionalProperties[tlsConnectionKey] != "true" ||
		details.AdditionalProperties[tlsCAKey] != "test-ca" {
		t.Fatalf("unexpected TLS connection details: %+v", details.AdditionalProperties)
	}
}

func TestBuildConnectionDetailsWaitsForTLSCA(t *testing.T) {
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{userPasswordSecretKey: []byte("password")},
	}
	_, err := buildConnectionDetails(newTLSContext(t, "", "", credentials))
	if !errors.Is(err, errTLSCABundleNotReady) {
		t.Fatalf("expected errTLSCABundleNotReady, got %v", err)
	}
}

func TestBuildConnectionDetailsOmitsTLSWhenDisabled(t *testing.T) {
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretName("test"), Namespace: "default"},
		Data:       map[string][]byte{userPasswordSecretKey: []byte("password")},
	}
	details, err := buildConnectionDetails(newTLSContext(t, "", `{"tls":{"enabled":false}}`, credentials))
	if err != nil {
		t.Fatalf("buildConnectionDetails() error = %v", err)
	}
	if len(details.AdditionalProperties) != 0 {
		t.Fatalf("expected no TLS connection details, got %+v", details.AdditionalProperties)
	}
}
