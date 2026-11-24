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

package webhookhandler

import (
	"fmt"

	"sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

// validateNetworkInterfaces checks that every interface of spec.network is an interface of the
// virtual machine template, which the CRD schema cannot express. It is meant for creation only:
// spec.network is immutable, and rejecting an update would also block the controller from
// removing the finalizer of a machine admitted before this check existed.
func validateNetworkInterfaces(spec *v1alpha1.KubevirtMachineSpec) error {
	if spec.Network == nil {
		return nil
	}

	templateInterfaces := map[string]bool{}
	if template := spec.VirtualMachineTemplate.Spec.Template; template != nil {
		for _, iface := range template.Spec.Domain.Devices.Interfaces {
			templateInterfaces[iface.Name] = true
		}
	}

	for i, iface := range spec.Network.Interfaces {
		if !templateInterfaces[iface.Name] {
			return fmt.Errorf("spec.network.interfaces[%d].name: interface %q is not defined in spec.virtualMachineTemplate.spec.template.spec.domain.devices.interfaces", i, iface.Name)
		}
	}
	return nil
}
