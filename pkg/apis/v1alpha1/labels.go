/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis"
)

func init() {
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(RestrictedLabelDomains...)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(
		LabelInstanceMemory,
		LabelInstanceCpu,
	)
}

var (
	CapacityGroup = "capacity." + Group

	LabelInstanceMemory = CapacityGroup + "/memory"
	LabelInstanceCpu    = CapacityGroup + "/cpu"

	// RestrictedLabelDomains are either prohibited by the kubelet or reserved by karpenter
	RestrictedLabelDomains = []string{
		Group,
	}

	AnnotationClusterAPINodeClassHash        = apis.Group + "/capinodeclass-hash"
	AnnotationClusterAPINodeClassHashVersion = apis.Group + "/capinodeclass-hash-version"
)
