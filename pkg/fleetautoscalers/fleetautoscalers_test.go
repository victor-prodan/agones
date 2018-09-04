/*
 * Copyright 2018 Google Inc. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package fleetautoscalers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeDesiredFleetSize(t *testing.T) {
	t.Parallel()

	fas, f := defaultFixtures()

	fas.Spec.BufferSize = 20
	f.Spec.Replicas = 50
	f.Status.Replicas = f.Spec.Replicas
	f.Status.AllocatedReplicas = 40
	f.Status.ReadyReplicas = 10
	
	replicas := computeDesiredFleetSize(fas, f)
	assert.Equal(t, replicas, int32(60))
	
	fas.Spec.MinReplicas = 65
	f.Spec.Replicas = 50
	f.Status.Replicas = f.Spec.Replicas
	f.Status.AllocatedReplicas = 40
	f.Status.ReadyReplicas = 10
	replicas = computeDesiredFleetSize(fas, f)
	assert.Equal(t, replicas, int32(65))
	
	fas.Spec.MinReplicas = 0
	fas.Spec.MaxReplicas = 55
	f.Spec.Replicas = 50
	f.Status.Replicas = f.Spec.Replicas
	f.Status.AllocatedReplicas = 40
	f.Status.ReadyReplicas = 10
	replicas = computeDesiredFleetSize(fas, f)
	assert.Equal(t, replicas, int32(55))
}
