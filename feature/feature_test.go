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

package feature

import (
	"testing"

	"github.com/spf13/pflag"
	capifeature "sigs.k8s.io/cluster-api/feature"
)

func TestKubevirtIPAMGate(t *testing.T) {
	if Gates.Enabled(KubevirtIPAM) {
		t.Fatal("KubevirtIPAM must be disabled by default")
	}

	// The manager's deployment sets the Cluster API gates in one flag; the config/components/ipam
	// component appends a second one.
	fs := pflag.NewFlagSet("manager", pflag.ContinueOnError)
	capifeature.MutableGates.AddFlag(fs)
	if err := fs.Parse([]string{"--feature-gates=MachinePool=false", "--feature-gates=KubevirtIPAM=true"}); err != nil {
		t.Fatal(err)
	}

	if !Gates.Enabled(KubevirtIPAM) {
		t.Error("KubevirtIPAM must be enabled by --feature-gates=KubevirtIPAM=true")
	}
	if Gates.Enabled(capifeature.MachinePool) {
		t.Error("an additional --feature-gates flag must keep the gates set by an earlier one")
	}
}
