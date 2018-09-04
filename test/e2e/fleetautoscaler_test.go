// Copyright 2018 Google Inc. All Rights Reserved.
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

package e2e

import (
	"fmt"
	"math/rand"
	"testing"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	e2e "agones.dev/agones/test/e2e/framework"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestAutoScalerBasicFunctions(t *testing.T) {
	t.Parallel()

	alpha1 := framework.AgonesClient.StableV1alpha1()
	fleets := alpha1.Fleets(defaultNs)
	flt, err := fleets.Create(defaultFleet())
	if assert.Nil(t, err) {
		defer fleets.Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
	}

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(flt.Spec.Replicas))
	assert.Nil(t, err, "fleet not ready")

	fleetautoscalers := alpha1.FleetAutoScalers(defaultNs)
	fas, err := fleetautoscalers.Create(defaultFleetAutoScaler(flt))
	if assert.Nil(t, err) {
		defer fleetautoscalers.Delete(fas.ObjectMeta.Name, nil) // nolint:errcheck
	}

	// the fleet shouautoscaler should scale the fleet up now up to BufferSize
	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(fas.Spec.BufferSize))
	assert.Nil(t, err, "fleet did not sync with autoscaler")

	// patch the autoscaler to increase MinReplicas and watch the fleet scale up
	fas, err = patchFleetAutoScaler(fas, fas.Spec.BufferSize, fas.Spec.BufferSize+2, fas.Spec.MaxReplicas)
	assert.Nil(t, err, "could not patch fleetautoscaler")

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(fas.Spec.BufferSize+2))
	assert.Nil(t, err, "fleet did not sync with autoscaler")

	// patch the autoscaler to remove MinReplicas and watch the fleet scale down
	fas, err = patchFleetAutoScaler(fas, fas.Spec.BufferSize, 0, fas.Spec.MaxReplicas)
	assert.Nil(t, err, "could not patch fleetautoscaler")

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(fas.Spec.BufferSize))
	assert.Nil(t, err, "fleet did not sync with autoscaler")

	// do an allocation and watch the fleet scale up
	fa := getAllocation(flt)
	fa, err = alpha1.FleetAllocations(defaultNs).Create(fa)
	assert.Nil(t, err)
	assert.Equal(t, v1alpha1.Allocated, fa.Status.GameServer.Status.State)
	err = framework.WaitForFleetCondition(flt, func(fleet *v1alpha1.Fleet) bool {
		return fleet.Status.AllocatedReplicas == 1
	})
	assert.Nil(t, err)

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(fas.Spec.BufferSize))
	assert.Nil(t, err, "fleet did not sync with autoscaler")

	// delete the allocated GameServer and watch the fleet scale down
	gp := int64(1)
	err = alpha1.GameServers(defaultNs).Delete(fa.Status.GameServer.ObjectMeta.Name, &metav1.DeleteOptions{GracePeriodSeconds: &gp})
	assert.Nil(t, err)
	err = framework.WaitForFleetCondition(flt, func(fleet *v1alpha1.Fleet) bool {
		return fleet.Status.AllocatedReplicas == 0 && fleet.Status.ReadyReplicas == fas.Spec.BufferSize && fleet.Status.Replicas == fas.Spec.BufferSize
	})
	assert.Nil(t, err)

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(fas.Spec.BufferSize))
	assert.Nil(t, err, "fleet did not sync with autoscaler")
}

// TestAutoScalerStressCreate creates many fleetautoscalers with random values
// to check if the creation validation works as expected and if the fleet scales
// to the expected number of replicas (when the creation is valid)
func TestAutoScalerStressCreate(t *testing.T) {
	//t.Parallel()

	alpha1 := framework.AgonesClient.StableV1alpha1()
	fleets := alpha1.Fleets(defaultNs)
	flt, err := fleets.Create(defaultFleet())
	if assert.Nil(t, err) {
		defer fleets.Delete(flt.ObjectMeta.Name, nil) // nolint:errcheck
	}

	err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(flt.Spec.Replicas))
	assert.Nil(t, err, "fleet not ready")

	r := rand.New(rand.NewSource(1783))

	fleetautoscalers := alpha1.FleetAutoScalers(defaultNs)
	for i := 0; i < 30; i++ {
		fas := defaultFleetAutoScaler(flt)
		fas.Spec.BufferSize = r.Int31n(10)
		fas.Spec.MinReplicas = r.Int31n(10)
		fas.Spec.MaxReplicas = r.Int31n(10)

		valid := fas.Spec.BufferSize > 0 &&
			fas.Spec.MaxReplicas > 0 &&
			fas.Spec.MaxReplicas >= fas.Spec.BufferSize &&
			fas.Spec.MinReplicas <= fas.Spec.MaxReplicas &&
			(fas.Spec.MinReplicas == 0 || fas.Spec.MinReplicas >= fas.Spec.BufferSize)

		fas, err := fleetautoscalers.Create(fas)
		if err == nil {
			assert.True(t, valid, fmt.Sprintf("FleetAutoscaler created even if the parameters are NOT valid: %d %d %d", fas.Spec.BufferSize, fas.Spec.MinReplicas, fas.Spec.MaxReplicas))

			expectedReplicas := fas.Spec.BufferSize
			if expectedReplicas < fas.Spec.MinReplicas {
				expectedReplicas = fas.Spec.MinReplicas
			}
			if expectedReplicas > fas.Spec.MaxReplicas {
				expectedReplicas = fas.Spec.MaxReplicas
			}
			// the fleet autoscaler should scale the fleet now to expectedReplicas
			err = framework.WaitForFleetCondition(flt, e2e.FleetReadyCount(expectedReplicas))
			assert.Nil(t, err, fmt.Sprintf("fleet did not sync with autoscaler, expected %d ready replicas", expectedReplicas))

			fleetautoscalers.Delete(fas.ObjectMeta.Name, nil) // nolint:errcheck
		} else {
			assert.False(t, valid, fmt.Sprintf("FleetAutoscaler NOT created even if the parameters are valid: %d %d %d", fas.Spec.BufferSize, fas.Spec.MinReplicas, fas.Spec.MaxReplicas))
		}
	}
}

// scaleFleet creates a patch to apply to a Fleet.
// easier for testing, as it removes object generational issues.
func patchFleetAutoScaler(fas *v1alpha1.FleetAutoScaler, bufferSize int32, minReplicas int32, maxReplicas int32) (*v1alpha1.FleetAutoScaler, error) {
	patch := fmt.Sprintf(
		`[{ "op": "replace", "path": "/spec/bufferSize", "value": %d },`+
			`{ "op": "replace", "path": "/spec/minReplicas", "value": %d },`+
			`{ "op": "replace", "path": "/spec/maxReplicas", "value": %d }]`,
		bufferSize, minReplicas, maxReplicas)
	logrus.
		WithField("fleetautoscaler", fas.ObjectMeta.Name).
		WithField("bufferSize", bufferSize).
		WithField("minReplicas", minReplicas).
		WithField("maxReplicas", maxReplicas).
		WithField("patch", patch).
		Info("Patching fleetautoscaler")

	return framework.AgonesClient.StableV1alpha1().FleetAutoScalers(defaultNs).Patch(fas.ObjectMeta.Name, types.JSONPatchType, []byte(patch))
}

// defaultFleetAutoScaler returns a default fleet autoscaler configuration for a given fleet
func defaultFleetAutoScaler(f *v1alpha1.Fleet) *v1alpha1.FleetAutoScaler {
	return &v1alpha1.FleetAutoScaler{
		ObjectMeta: metav1.ObjectMeta{Name: f.ObjectMeta.Name + "-autoscaler", Namespace: defaultNs},
		Spec: v1alpha1.FleetAutoScalerSpec{
			FleetName:   f.ObjectMeta.Name,
			BufferSize:  5,
			MaxReplicas: 20,
		},
	}
}

func getAllocation(f *v1alpha1.Fleet) *v1alpha1.FleetAllocation {
	// get an allocation
	return &v1alpha1.FleetAllocation{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "allocation-", Namespace: defaultNs},
		Spec: v1alpha1.FleetAllocationSpec{
			FleetName: f.ObjectMeta.Name,
		},
	}
}
