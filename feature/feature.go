/*
Copyright 2026 The Kubernetes Authors.

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

// Package feature defines the feature gates of the KubeVirt infrastructure provider.
//
// The gates are added to the Cluster API feature gate set, so that the manager keeps a single
// --feature-gates flag for the Cluster API gates and its own.
package feature

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
	capifeature "sigs.k8s.io/cluster-api/feature"
)

const (
	// Every feature gate should add method here following this template:
	//
	// // owner: @username
	// // alpha: v1.X
	// MyFeature featuregate.Feature = "MyFeature".

	// KubevirtIPAM enables static guest addresses from Cluster API IPAM pools, configured through
	// KubevirtMachine spec.network. With the gate enabled, the KubevirtMachine controller watches
	// IPAddressClaims and needs the ipam.cluster.x-k8s.io RBAC rules of the
	// config/components/ipam kustomize component. With the gate disabled, the controller neither
	// watches nor reads any ipam.cluster.x-k8s.io resource, and a KubevirtMachine with
	// spec.network is not provisioned.
	//
	// alpha: v0.12
	KubevirtIPAM featuregate.Feature = "KubevirtIPAM"
)

func init() {
	runtime.Must(capifeature.MutableGates.Add(defaultKubevirtFeatureGates))
}

// defaultKubevirtFeatureGates consists of all known KubeVirt provider feature keys.
// To add a new feature, define a key for it above and add it here.
var defaultKubevirtFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	// Every feature should be initiated here:
	KubevirtIPAM: {Default: false, PreRelease: featuregate.Alpha},
}

// Gates is a shared global FeatureGate, holding the Cluster API gates and the gates above. The
// manager sets them from its --feature-gates flag.
var Gates featuregate.FeatureGate = capifeature.Gates
