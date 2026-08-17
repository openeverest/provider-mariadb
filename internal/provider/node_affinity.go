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
	"fmt"
	"strings"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// buildAffinity assembles the operator's AffinityConfig from the raw engine affinity
// (an escape hatch for full corev1.Affinity), the parsed node-targeting rules, and the
// Galera default pod anti-affinity. Node targeting is merged with — not a replacement
// for — the Galera anti-affinity so HA pod spreading is preserved.
func buildAffinity(
	raw *corev1.Affinity,
	nodeAffinityRules string,
	galera bool,
	instanceName string,
) (*mariadbv1alpha1.AffinityConfig, error) {
	cfg := convertAffinity(raw)

	if strings.TrimSpace(nodeAffinityRules) != "" {
		na, err := buildRequiredNodeAffinity(nodeAffinityRules)
		if err != nil {
			return nil, err
		}
		if na != nil {
			if cfg == nil {
				cfg = &mariadbv1alpha1.AffinityConfig{}
			}
			cfg.NodeAffinity = na
		}
	}

	if galera {
		antiAffinity := defaultGaleraAffinity(instanceName).PodAntiAffinity
		switch {
		case cfg == nil:
			cfg = &mariadbv1alpha1.AffinityConfig{
				Affinity: mariadbv1alpha1.Affinity{PodAntiAffinity: antiAffinity},
			}
		case cfg.PodAntiAffinity == nil:
			cfg.PodAntiAffinity = antiAffinity
		}
	}

	return cfg, nil
}

// buildRequiredNodeAffinity compiles the node-targeting rules into a required node
// affinity whose single term ANDs all rules together. Returns nil when there are no rules.
func buildRequiredNodeAffinity(rules string) (*mariadbv1alpha1.NodeAffinity, error) {
	reqs, err := parseNodeAffinityRules(rules)
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, nil
	}
	return &mariadbv1alpha1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &mariadbv1alpha1.NodeSelector{
			NodeSelectorTerms: []mariadbv1alpha1.NodeSelectorTerm{
				{MatchExpressions: reqs},
			},
		},
	}, nil
}

// parseNodeAffinityRules parses newline-separated node label rules into node selector
// requirements. Each non-empty line is "<key> <operator> [<v1>,<v2>...]" where operator
// is In, NotIn, Exists or DoesNotExist. In/NotIn require at least one value;
// Exists/DoesNotExist must have none.
func parseNodeAffinityRules(s string) ([]mariadbv1alpha1.NodeSelectorRequirement, error) {
	var reqs []mariadbv1alpha1.NodeSelectorRequirement
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: expected '<key> <operator> [values]', got %q", i+1, line)
		}
		op, err := parseNodeSelectorOperator(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		var values []string
		if len(fields) > 2 {
			values = splitValues(strings.Join(fields[2:], ""))
		}
		switch op {
		case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn:
			if len(values) == 0 {
				return nil, fmt.Errorf("line %d: operator %q requires at least one value", i+1, op)
			}
		case corev1.NodeSelectorOpExists, corev1.NodeSelectorOpDoesNotExist:
			if len(values) > 0 {
				return nil, fmt.Errorf("line %d: operator %q must not have values", i+1, op)
			}
		}
		reqs = append(reqs, mariadbv1alpha1.NodeSelectorRequirement{
			Key:      fields[0],
			Operator: op,
			Values:   values,
		})
	}
	return reqs, nil
}

// parseNodeSelectorOperator maps a case-insensitive operator token onto its
// corev1.NodeSelectorOperator value.
func parseNodeSelectorOperator(s string) (corev1.NodeSelectorOperator, error) {
	switch strings.ToLower(s) {
	case "in":
		return corev1.NodeSelectorOpIn, nil
	case "notin":
		return corev1.NodeSelectorOpNotIn, nil
	case "exists":
		return corev1.NodeSelectorOpExists, nil
	case "doesnotexist":
		return corev1.NodeSelectorOpDoesNotExist, nil
	default:
		return "", fmt.Errorf("unsupported operator %q; use In, NotIn, Exists or DoesNotExist", s)
	}
}

// splitValues splits a comma-separated value list, trimming blanks and dropping empties.
func splitValues(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
