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

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openeverest/provider-mariadb/internal/common"
)

// convertAffinity maps a standard Kubernetes corev1.Affinity supplied on the
// Instance's engine component onto the mariadb-operator's AffinityConfig.
//
// The operator uses a trimmed-down affinity type that only models NodeAffinity
// and PodAntiAffinity; PodAffinity (pod co-location) is not representable and is
// rejected by validateAffinity before this runs. Pod (anti-)affinity terms only
// carry a LabelSelector and TopologyKey on the operator side, so richer term
// fields (Namespaces, NamespaceSelector, MatchLabelKeys, ...) are not mapped.
//
// It returns nil when there is nothing the operator can honor, so the operator
// keeps its own defaults instead of receiving an empty affinity block.
func convertAffinity(a *corev1.Affinity) *mariadbv1alpha1.AffinityConfig {
	if a == nil {
		return nil
	}

	cfg := &mariadbv1alpha1.AffinityConfig{}
	if a.NodeAffinity != nil {
		cfg.NodeAffinity = convertNodeAffinity(a.NodeAffinity)
	}
	if a.PodAntiAffinity != nil {
		cfg.PodAntiAffinity = convertPodAntiAffinity(a.PodAntiAffinity)
	}

	if cfg.NodeAffinity == nil && cfg.PodAntiAffinity == nil {
		return nil
	}
	return cfg
}

// validateAffinity rejects affinity rules the operator cannot express. Only
// PodAffinity is unsupported; NodeAffinity and PodAntiAffinity map cleanly.
func validateAffinity(a *corev1.Affinity) error {
	if a == nil {
		return nil
	}
	if a.PodAffinity != nil {
		return fmt.Errorf(
			"spec.components.%s.affinity.podAffinity is not supported by the MariaDB operator; "+
				"only nodeAffinity and podAntiAffinity are supported",
			common.ComponentEngine,
		)
	}
	return nil
}

func convertNodeAffinity(na *corev1.NodeAffinity) *mariadbv1alpha1.NodeAffinity {
	out := &mariadbv1alpha1.NodeAffinity{}
	if na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		out.RequiredDuringSchedulingIgnoredDuringExecution =
			convertNodeSelector(na.RequiredDuringSchedulingIgnoredDuringExecution)
	}
	for _, term := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		out.PreferredDuringSchedulingIgnoredDuringExecution = append(
			out.PreferredDuringSchedulingIgnoredDuringExecution,
			mariadbv1alpha1.PreferredSchedulingTerm{
				Weight:     term.Weight,
				Preference: convertNodeSelectorTerm(term.Preference),
			},
		)
	}
	return out
}

func convertNodeSelector(ns *corev1.NodeSelector) *mariadbv1alpha1.NodeSelector {
	out := &mariadbv1alpha1.NodeSelector{}
	for _, term := range ns.NodeSelectorTerms {
		out.NodeSelectorTerms = append(out.NodeSelectorTerms, convertNodeSelectorTerm(term))
	}
	return out
}

func convertNodeSelectorTerm(term corev1.NodeSelectorTerm) mariadbv1alpha1.NodeSelectorTerm {
	out := mariadbv1alpha1.NodeSelectorTerm{}
	for _, req := range term.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, convertNodeSelectorRequirement(req))
	}
	for _, req := range term.MatchFields {
		out.MatchFields = append(out.MatchFields, convertNodeSelectorRequirement(req))
	}
	return out
}

func convertNodeSelectorRequirement(req corev1.NodeSelectorRequirement) mariadbv1alpha1.NodeSelectorRequirement {
	return mariadbv1alpha1.NodeSelectorRequirement{
		Key:      req.Key,
		Operator: req.Operator,
		Values:   req.Values,
	}
}

func convertPodAntiAffinity(paa *corev1.PodAntiAffinity) *mariadbv1alpha1.PodAntiAffinity {
	out := &mariadbv1alpha1.PodAntiAffinity{}
	for _, term := range paa.RequiredDuringSchedulingIgnoredDuringExecution {
		out.RequiredDuringSchedulingIgnoredDuringExecution = append(
			out.RequiredDuringSchedulingIgnoredDuringExecution,
			convertPodAffinityTerm(term),
		)
	}
	for _, term := range paa.PreferredDuringSchedulingIgnoredDuringExecution {
		out.PreferredDuringSchedulingIgnoredDuringExecution = append(
			out.PreferredDuringSchedulingIgnoredDuringExecution,
			mariadbv1alpha1.WeightedPodAffinityTerm{
				Weight:          term.Weight,
				PodAffinityTerm: convertPodAffinityTerm(term.PodAffinityTerm),
			},
		)
	}
	return out
}

func convertPodAffinityTerm(term corev1.PodAffinityTerm) mariadbv1alpha1.PodAffinityTerm {
	out := mariadbv1alpha1.PodAffinityTerm{
		TopologyKey: term.TopologyKey,
	}
	if term.LabelSelector != nil {
		out.LabelSelector = convertLabelSelector(term.LabelSelector)
	}
	return out
}

func convertLabelSelector(ls *metav1.LabelSelector) *mariadbv1alpha1.LabelSelector {
	out := &mariadbv1alpha1.LabelSelector{
		MatchLabels: ls.MatchLabels,
	}
	for _, req := range ls.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, mariadbv1alpha1.LabelSelectorRequirement{
			Key:      req.Key,
			Operator: req.Operator,
			Values:   req.Values,
		})
	}
	return out
}
