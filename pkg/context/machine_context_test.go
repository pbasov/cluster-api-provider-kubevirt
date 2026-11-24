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

package context

import (
	gocontext "context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

func TestReadySummarizesIPAddressClaimed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		claimed   *metav1.Condition
		wantReady metav1.ConditionStatus
	}{
		{name: "without spec.network", wantReady: metav1.ConditionTrue},
		{
			name:      "addresses allocated",
			claimed:   &metav1.Condition{Type: infrav1.IPAddressClaimedCondition, Status: metav1.ConditionTrue, Reason: infrav1.IPAddressClaimedReason, Message: "allocated"},
			wantReady: metav1.ConditionTrue,
		},
		{
			name:      "waiting for addresses",
			claimed:   &metav1.Condition{Type: infrav1.IPAddressClaimedCondition, Status: metav1.ConditionFalse, Reason: infrav1.WaitingForIPAddressReason, Message: "Waiting for IPAddressClaims: m-lan-0"},
			wantReady: metav1.ConditionFalse,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := &infrav1.KubevirtMachine{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
			scheme := runtime.NewScheme()
			if err := infrav1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).WithStatusSubresource(machine).Build()
			helper, err := patch.NewHelper(machine, c)
			if err != nil {
				t.Fatal(err)
			}

			conditions.Set(machine, metav1.Condition{Type: infrav1.VMProvisionedCondition, Status: metav1.ConditionTrue, Reason: "VMProvisioned", Message: "provisioned"})
			conditions.Set(machine, metav1.Condition{Type: infrav1.BootstrapExecSucceededCondition, Status: metav1.ConditionTrue, Reason: "BootstrapExecSucceeded", Message: "bootstrapped"})
			if tc.claimed != nil {
				conditions.Set(machine, *tc.claimed)
			}

			machineContext := &MachineContext{Context: gocontext.Background(), KubevirtMachine: machine}
			if err := machineContext.PatchKubevirtMachine(helper); err != nil {
				t.Fatal(err)
			}

			ready := conditions.Get(machine, clusterv1.ReadyCondition)
			if ready == nil || ready.Status != tc.wantReady {
				t.Fatalf("expected Ready %s, got %+v", tc.wantReady, ready)
			}
			if tc.wantReady == metav1.ConditionFalse && !strings.Contains(ready.Message, tc.claimed.Message) {
				t.Errorf("expected the Ready message to tell why addresses are missing, got %q", ready.Message)
			}
		})
	}
}

func TestEnsureNonEmptyConditionMessages(t *testing.T) {
	machine := &infrav1.KubevirtMachine{
		Status: infrav1.KubevirtMachineStatus{
			Conditions: []metav1.Condition{
				{Type: "Ready", Reason: "Ready", Message: "already set"},
				{Type: "VMProvisioned", Reason: "VMProvisioned"},
				{Type: "BootstrapExecSucceeded"},
			},
		},
	}

	ensureNonEmptyConditionMessages(machine)

	conditions := machine.Status.Conditions
	if conditions[0].Message != "already set" {
		t.Fatalf("existing message was changed: %q", conditions[0].Message)
	}
	if conditions[1].Message != conditions[1].Reason {
		t.Fatalf("missing message was not derived from reason: %q", conditions[1].Message)
	}
	if conditions[2].Reason == "" || conditions[2].Message == "" {
		t.Fatalf("missing reason and message were not defaulted: %#v", conditions[2])
	}
}
