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
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
)

// computeDesiredFleetSize computes the new desired size of the given fleet
func computeDesiredFleetSize(fas *stablev1alpha1.FleetAutoScaler, f *stablev1alpha1.Fleet) (int32, bool) {

	replicas := f.Status.AllocatedReplicas + fas.Spec.BufferSize
	limited := false
	if fas.Spec.MinReplicas > 0 && replicas < fas.Spec.MinReplicas {
		replicas = fas.Spec.MinReplicas
		limited = true
	}
	if fas.Spec.MaxReplicas > 0 && replicas > fas.Spec.MaxReplicas {
		replicas = fas.Spec.MaxReplicas
		limited = true
	}

	return replicas, limited
}
