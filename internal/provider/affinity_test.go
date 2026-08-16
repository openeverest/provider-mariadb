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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConvertAffinity_Nil(t *testing.T) {
	if got := convertAffinity(nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestConvertAffinity_PodAffinityOnly_ReturnsNil(t *testing.T) {
	// PodAffinity is unsupported and dropped; with nothing else to map the
	// converter returns nil so the operator keeps its defaults.
	in := &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{TopologyKey: "kubernetes.io/hostname"},
			},
		},
	}
	if got := convertAffinity(in); got != nil {
		t.Fatalf("expected nil (PodAffinity is not mappable), got %+v", got)
	}
}

func TestConvertAffinity_PodAntiAffinity(t *testing.T) {
	in := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "mariadb"},
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "app.kubernetes.io/instance",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"my-instance"},
							},
						},
					},
				},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 50,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: "topology.kubernetes.io/zone",
					},
				},
			},
		},
	}

	got := convertAffinity(in)
	if got == nil || got.PodAntiAffinity == nil {
		t.Fatalf("expected PodAntiAffinity to be set, got %+v", got)
	}
	required := got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required) != 1 {
		t.Fatalf("expected 1 required term, got %d", len(required))
	}
	if required[0].TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("unexpected topology key: %q", required[0].TopologyKey)
	}
	if required[0].LabelSelector == nil {
		t.Fatalf("expected label selector to be mapped")
	}
	if required[0].LabelSelector.MatchLabels["app"] != "mariadb" {
		t.Errorf("match labels not mapped: %+v", required[0].LabelSelector.MatchLabels)
	}
	if len(required[0].LabelSelector.MatchExpressions) != 1 ||
		required[0].LabelSelector.MatchExpressions[0].Key != "app.kubernetes.io/instance" {
		t.Errorf("match expressions not mapped: %+v", required[0].LabelSelector.MatchExpressions)
	}

	preferred := got.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) != 1 || preferred[0].Weight != 50 {
		t.Fatalf("expected 1 preferred term with weight 50, got %+v", preferred)
	}
	if preferred[0].PodAffinityTerm.TopologyKey != "topology.kubernetes.io/zone" {
		t.Errorf("unexpected preferred topology key: %q", preferred[0].PodAffinityTerm.TopologyKey)
	}
}

func TestConvertAffinity_NodeAffinity(t *testing.T) {
	in := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "disktype",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"ssd"},
							},
						},
					},
				},
			},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 10,
					Preference: corev1.NodeSelectorTerm{
						MatchFields: []corev1.NodeSelectorRequirement{
							{
								Key:      "metadata.name",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"node-1"},
							},
						},
					},
				},
			},
		},
	}

	got := convertAffinity(in)
	if got == nil || got.NodeAffinity == nil {
		t.Fatalf("expected NodeAffinity to be set, got %+v", got)
	}
	required := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) != 1 {
		t.Fatalf("expected 1 node selector term, got %+v", required)
	}
	if got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.
		NodeSelectorTerms[0].MatchExpressions[0].Key != "disktype" {
		t.Errorf("node match expression not mapped")
	}

	preferred := got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) != 1 || preferred[0].Weight != 10 {
		t.Fatalf("expected 1 preferred term with weight 10, got %+v", preferred)
	}
	if preferred[0].Preference.MatchFields[0].Key != "metadata.name" {
		t.Errorf("node match field not mapped")
	}
}

func TestValidateAffinity(t *testing.T) {
	tests := []struct {
		name    string
		in      *corev1.Affinity
		wantErr bool
	}{
		{name: "nil", in: nil, wantErr: false},
		{
			name:    "node affinity ok",
			in:      &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
			wantErr: false,
		},
		{
			name:    "pod anti-affinity ok",
			in:      &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}},
			wantErr: false,
		},
		{
			name:    "pod affinity rejected",
			in:      &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAffinity(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAffinity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
