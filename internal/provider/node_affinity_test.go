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

func TestParseNodeAffinityRules(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "empty is no rules", input: "", want: 0},
		{name: "blank lines skipped", input: "\n  \n", want: 0},
		{name: "single In rule", input: "disktype In ssd,nvme", want: 1},
		{name: "multiple rules", input: "disktype In ssd\ngpu Exists\nzone NotIn eu", want: 3},
		{name: "operator case-insensitive", input: "disktype in ssd", want: 1},
		{name: "values tolerate spaces", input: "disktype In ssd, nvme", want: 1},
		{name: "exists without values", input: "gpu Exists", want: 1},
		{name: "doesnotexist without values", input: "gpu DoesNotExist", want: 1},
		{name: "in requires values", input: "disktype In", wantErr: true},
		{name: "notin requires values", input: "disktype NotIn", wantErr: true},
		{name: "exists rejects values", input: "gpu Exists ssd", wantErr: true},
		{name: "doesnotexist rejects values", input: "gpu DoesNotExist ssd", wantErr: true},
		{name: "missing operator", input: "disktype", wantErr: true},
		{name: "unknown operator", input: "disktype Equals ssd", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNodeAffinityRules(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseNodeAffinityRules() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && len(got) != tt.want {
				t.Fatalf("parseNodeAffinityRules() got %d rules, want %d", len(got), tt.want)
			}
		})
	}
}

func TestParseNodeAffinityRules_ValuesAndOperator(t *testing.T) {
	got, err := parseNodeAffinityRules("disktype In ssd, nvme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 requirement, got %d", len(got))
	}
	req := got[0]
	if req.Key != "disktype" || req.Operator != corev1.NodeSelectorOpIn {
		t.Fatalf("unexpected key/operator: %+v", req)
	}
	if len(req.Values) != 2 || req.Values[0] != "ssd" || req.Values[1] != "nvme" {
		t.Fatalf("unexpected values: %+v", req.Values)
	}
}

func TestBuildAffinity_StandaloneNoRules(t *testing.T) {
	got, err := buildAffinity(nil, "", false, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil affinity, got %+v", got)
	}
}

func TestBuildAffinity_GaleraDefaultsAntiAffinity(t *testing.T) {
	got, err := buildAffinity(nil, "", true, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.PodAntiAffinity == nil {
		t.Fatalf("expected pod anti-affinity, got %+v", got)
	}
	if got.NodeAffinity != nil {
		t.Fatalf("expected no node affinity, got %+v", got.NodeAffinity)
	}
}

func TestBuildAffinity_StandaloneWithRules(t *testing.T) {
	got, err := buildAffinity(nil, "disktype In ssd", false, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.NodeAffinity == nil {
		t.Fatalf("expected node affinity, got %+v", got)
	}
	if got.PodAntiAffinity != nil {
		t.Fatalf("standalone must not get anti-affinity, got %+v", got.PodAntiAffinity)
	}
}

// Galera + node targeting must keep BOTH: node affinity and the default HA anti-affinity.
func TestBuildAffinity_GaleraWithRulesMerges(t *testing.T) {
	got, err := buildAffinity(nil, "disktype In ssd", true, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.NodeAffinity == nil || got.PodAntiAffinity == nil {
		t.Fatalf("expected node affinity and anti-affinity, got %+v", got)
	}
	req := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) != 1 || len(req.NodeSelectorTerms[0].MatchExpressions) != 1 {
		t.Fatalf("unexpected node affinity shape: %+v", got.NodeAffinity)
	}
}

// A user-supplied pod anti-affinity must not be replaced by the Galera default.
func TestBuildAffinity_GaleraKeepsRawAntiAffinity(t *testing.T) {
	raw := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "mariadb"},
					},
				},
			},
		},
	}
	got, err := buildAffinity(raw, "", true, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.PodAntiAffinity == nil {
		t.Fatalf("expected pod anti-affinity, got %+v", got)
	}
	if len(got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("expected the raw required anti-affinity to be preserved, got %+v", got.PodAntiAffinity)
	}
}

func TestBuildAffinity_ParseErrorPropagates(t *testing.T) {
	if _, err := buildAffinity(nil, "disktype Equals ssd", false, "test"); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}
