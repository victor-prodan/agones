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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FleetAutoScaler is the data structure for a FleetAutoScaler resource
type FleetAutoScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetAutoScalerSpec   `json:"spec"`
	Status FleetAutoScalerStatus `json:"status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// FleetAutoScalerList is a list of Fleet Scaler resources
type FleetAutoScalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FleetAutoScaler `json:"items"`
}

// FleetAutoScalerSpec is the spec for a Fleet Scaler
type FleetAutoScalerSpec struct {
	FleetName string `json:"fleetName"`
	// MaxReplicas is the maximum amount of replicas that the fleet may have.
	// It must be bigger than both MinReplicas and BufferSize
	MaxReplicas int32 `json:"maxReplicas"`

	// MinReplicas is the minimum amount of replicas that the fleet must have
	// If zero, it is ignored.
	// If non zero, it must be smaller than MaxReplicas and bigger than BufferSize
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// BufferSize defines how many replicas the autoscaler tries to have ready all the time
	// Must be bigger than 0
	// Note: by "ready" we understand in this case "non-allocated"; this is done to ensure robustness
	//       and computation stability in different edge case (fleet just created, not enough capacity in the cluster etc)
	BufferSize int32 `json:"bufferSize"`
}

// FleetAutoScalerStatus needs doc
type FleetAutoScalerStatus struct {
	// CurrentReplicas is the current number of gameserver replicas
	// of the fleet managed by this autoscaler, as last seen by the autoscaler
	CurrentReplicas int32 `json:"currentReplicas"`

	// DesiredReplicas is the desired number of gameserver replicas
	// of the fleet managed by this autoscaler, as last calculated by the autoscaler
	DesiredReplicas int32 `json:"desiredReplicas"`

	// lastScaleTime is the last time the FleetAutoScaler scaled the attached fleet,
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// AbleToScale indicates that we can access the target fleet
	AbleToScale bool `json:"ableToScale,omitempty"`

	// ScalingLimited indicates that the calculated scale would be above or below the range
	// defined by MinReplicas and MaxReplicas, and has thus been capped.
	ScalingLimited bool `json:"scalingLimited,omitempty"`
}

// ValidateUpdate validates when an update occurs
func (fas *FleetAutoScaler) ValidateUpdate(new *FleetAutoScaler, causes []metav1.StatusCause) []metav1.StatusCause {
	if fas.Spec.FleetName != new.Spec.FleetName {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Field:   "fleetName",
			Message: "fleetName cannot be updated",
		})
	}

	return new.ValidateAutoScalingSettings(causes)
}

//ValidateAutoScalingSettings validates the FleetAutoScaler scaling settings
func (fas *FleetAutoScaler) ValidateAutoScalingSettings(causes []metav1.StatusCause) []metav1.StatusCause {
	if fas.Spec.MinReplicas != 0 && fas.Spec.MinReplicas > fas.Spec.MaxReplicas {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Field:   "minReplicas",
			Message: "MinReplicas is bigger than MaxReplicas",
		})
	}
	if fas.Spec.BufferSize <= 0 {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Field:   "bufferSize",
			Message: "BufferSize must be bigger than 0",
		})
	}
	if fas.Spec.MaxReplicas < fas.Spec.BufferSize {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Field:   "maxReplicas",
			Message: "MaxReplicas is smaller than BufferSize",
		})
	}
	if fas.Spec.MinReplicas != 0 && fas.Spec.MinReplicas < fas.Spec.BufferSize {
		causes = append(causes, metav1.StatusCause{
			Type:    metav1.CauseTypeFieldValueInvalid,
			Field:   "minReplicas",
			Message: "minReplicas is smaller than BufferSize",
		})
	}
	return causes
}
