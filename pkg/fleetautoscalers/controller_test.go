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

package fleetautoscalers

import (
	"encoding/json"
	"fmt"
	"testing"

	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	agtesting "agones.dev/agones/pkg/testing"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/stretchr/testify/assert"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

var (
	gvk = metav1.GroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("FleetAutoScaler"))
)

func TestControllerCreationMutationHandler(t *testing.T) {
	t.Parallel()
	fs, f := defaultFixtures()
	c, m := newFakeController()

	m.AgonesClient.AddReactor("list", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &v1alpha1.FleetList{Items: []v1alpha1.Fleet{*f}}, nil
	})

	_, cancel := agtesting.StartInformers(m)
	defer cancel()

	review, err := newAdmissionReview(*fs)
	assert.Nil(t, err)

	result, err := c.creationMutationHandler(review)
	assert.Nil(t, err)
	assert.True(t, result.Response.Allowed, fmt.Sprintf("%#v", result.Response))
	assert.Equal(t, admv1beta1.PatchTypeJSONPatch, *result.Response.PatchType)
	assert.Contains(t, string(result.Response.Patch), "/status/Fleet")
	assert.Contains(t, string(result.Response.Patch), "/metadata/ownerReferences")
}

func TestControllerCreationValidationHandler(t *testing.T) {
	t.Parallel()

	c, _ := newFakeController()

	t.Run("fleet scaler has a fleet", func(t *testing.T) {
		fs := v1alpha1.FleetAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: "fas-1", Namespace: "default"},
			Spec:   v1alpha1.FleetAutoScalerSpec{FleetName: "doesnotexist"},
			Status: v1alpha1.FleetAutoScalerStatus{Fleet: &v1alpha1.Fleet{}},
		}

		review, err := newAdmissionReview(fs)
		assert.Nil(t, err)

		result, err := c.creationValidationHandler(review)
		assert.Nil(t, err)
		assert.True(t, result.Response.Allowed)
	})

	t.Run("fleet scaler does not have a fleet", func(t *testing.T) {
		fs := v1alpha1.FleetAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: "fas-1", Namespace: "default"},
			Spec: v1alpha1.FleetAutoScalerSpec{FleetName: "doesnotexist"},
		}

		review, err := newAdmissionReview(fs)
		assert.Nil(t, err)

		result, err := c.creationValidationHandler(review)
		assert.Nil(t, err)
		assert.False(t, result.Response.Allowed)
		assert.Equal(t, "fleetName", result.Response.Result.Details.Causes[0].Field)
	})
}

func TestControllerMutationValidationHandler(t *testing.T) {
	t.Parallel()
	c, _ := newFakeController()

	fs := v1alpha1.FleetAutoScaler{ObjectMeta: metav1.ObjectMeta{Name: "fas-1", Namespace: "default"},
		Spec: v1alpha1.FleetAutoScalerSpec{FleetName: "my-fleet-name", BufferSize: 1},
	}

	t.Run("same fleetName", func(t *testing.T) {
		review, err := newAdmissionReview(fs)
		assert.Nil(t, err)
		review.Request.OldObject = *review.Request.Object.DeepCopy()

		result, err := c.mutationValidationHandler(review)
		assert.Nil(t, err)
		assert.True(t, result.Response.Allowed)
	})

	t.Run("different fleetname", func(t *testing.T) {
		review, err := newAdmissionReview(fs)
		assert.Nil(t, err)
		oldObject := fs.DeepCopy()
		oldObject.Spec.FleetName = "changed"

		json, err := json.Marshal(oldObject)
		assert.Nil(t, err)
		review.Request.OldObject = runtime.RawExtension{Raw: json}

		result, err := c.mutationValidationHandler(review)
		assert.Nil(t, err)
		assert.False(t, result.Response.Allowed)
		assert.Equal(t, metav1.StatusReasonInvalid, result.Response.Result.Reason)
		assert.NotNil(t, result.Response.Result.Details)
	})
}

func TestControllerSyncFleetAutoScaler(t *testing.T) {
	t.Parallel()

	t.Run("scaling up", func(t *testing.T) {
		t.Parallel()
		c, m := newFakeController()
		fas, f := defaultFixtures()
		fas.Status.Fleet = f
		fas.Spec.BufferSize = 7
		
		f.Spec.Replicas = 5
		f.Status.Replicas = 5
		f.Status.AllocatedReplicas = 5
		f.Status.ReadyReplicas = 0

		updated := false
		
		m.AgonesClient.AddReactor("list", "fleetautoscalers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.FleetAutoScalerList{Items: []v1alpha1.FleetAutoScaler{*fas}}, nil
		})

		m.AgonesClient.AddReactor("list", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.FleetList{Items: []v1alpha1.Fleet{*f}}, nil
		})

		m.AgonesClient.AddReactor("update", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			ca := action.(k8stesting.UpdateAction)
			f := ca.GetObject().(*v1alpha1.Fleet)
			assert.Equal(t, f.Spec.Replicas, f.Status.AllocatedReplicas + fas.Spec.BufferSize)

			return true, f, nil
		})

		_, cancel := agtesting.StartInformers(m, c.fleetAutoScalerSynced)
		defer cancel()

		err := c.syncFleetAutoScaler("default/fas-1")
		assert.Nil(t, err)
		assert.True(t, updated, "fleet should have been updated")
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "AutoScalingFleet")
	})

	t.Run("scaling down", func(t *testing.T) {
		t.Parallel()
		c, m := newFakeController()
		fas, f := defaultFixtures()
		fas.Status.Fleet = f
		fas.Spec.BufferSize = 8;
		
		f.Spec.Replicas = 20
		f.Status.Replicas = 20
		f.Status.AllocatedReplicas = 5
		f.Status.ReadyReplicas = 15

		updated := false
		
		m.AgonesClient.AddReactor("list", "fleetautoscalers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.FleetAutoScalerList{Items: []v1alpha1.FleetAutoScaler{*fas}}, nil
		})

		m.AgonesClient.AddReactor("list", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.FleetList{Items: []v1alpha1.Fleet{*f}}, nil
		})

		m.AgonesClient.AddReactor("update", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			updated = true
			ca := action.(k8stesting.UpdateAction)
			f := ca.GetObject().(*v1alpha1.Fleet)
			assert.Equal(t, f.Spec.Replicas, f.Status.AllocatedReplicas + fas.Spec.BufferSize)

			return true, f, nil
		})

		_, cancel := agtesting.StartInformers(m, c.fleetAutoScalerSynced)
		defer cancel()

		err := c.syncFleetAutoScaler("default/fas-1")
		assert.Nil(t, err)
		assert.True(t, updated, "fleet should have been updated")
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "AutoScalingFleet")
	})

	t.Run("no scaling", func(t *testing.T) {
		t.Parallel()
		c, m := newFakeController()
		fas, f := defaultFixtures()
		f.Spec.Replicas = f.Status.AllocatedReplicas + 5
		
		m.AgonesClient.AddReactor("list", "fleetautoscalers", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1alpha1.FleetAutoScalerList{Items: []v1alpha1.FleetAutoScaler{*fas}}, nil
		})

		m.AgonesClient.AddReactor("update", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			assert.FailNow(t, "should not update")
			return false, nil, nil
		})

		_, cancel := agtesting.StartInformers(m, c.fleetAutoScalerSynced)
		defer cancel()

		err := c.syncFleetAutoScaler(fas.ObjectMeta.Name)
		assert.Nil(t, err)
		agtesting.AssertNoEvent(t, m.FakeRecorder.Events)
	})
}

func TestControllerScaleFleet(t *testing.T) {
	t.Parallel()
	
	t.Run("fleet that must be scaled", func(t *testing.T) {
		c, m := newFakeController()
		fas,f := defaultFixtures()
		replicas := f.Spec.Replicas + 5

		update := false

		m.AgonesClient.AddReactor("update", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			update = true
			ca := action.(k8stesting.UpdateAction)
			f := ca.GetObject().(*v1alpha1.Fleet)
			assert.Equal(t, replicas, f.Spec.Replicas)

			return true, f, nil
		})
			
		err := c.scaleFleet(fas, f, replicas)
		assert.Nil(t, err)
		assert.True(t, update, "Should be update")
		agtesting.AssertEventContains(t, m.FakeRecorder.Events, "ScalingFleet")
	})
	
	t.Run("noop", func(t *testing.T) {
		c, m := newFakeController()
		fas, f := defaultFixtures()
		replicas := f.Spec.Replicas

		m.AgonesClient.AddReactor("update", "fleets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			assert.FailNow(t, "should not update")
			return false, nil, nil
		})
			
		err := c.scaleFleet(fas, f, replicas)
		assert.Nil(t, err)
		agtesting.AssertNoEvent(t, m.FakeRecorder.Events)
	})
}


func defaultFixtures() (*v1alpha1.FleetAutoScaler, *v1alpha1.Fleet) {
	f := &v1alpha1.Fleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fleet-1",
			Namespace: "default",
			UID:       "1234",
		},
		Spec: v1alpha1.FleetSpec{
			Replicas: 5,
			Template: v1alpha1.GameServerTemplateSpec{},
		},
		Status: v1alpha1.FleetStatus{
			Replicas: 5,
			ReadyReplicas: 3,
			AllocatedReplicas: 2,
		},
	}
	
	fs := &v1alpha1.FleetAutoScaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fas-1",
			Namespace: "default",
		},
		Spec: v1alpha1.FleetAutoScalerSpec{
			FleetName: f.ObjectMeta.Name,
			BufferSize: 5,
		},
	}

	return fs, f
}

// newFakeController returns a controller, backed by the fake Clientset
func newFakeController() (*Controller, agtesting.Mocks) {
	m := agtesting.NewMocks()
	wh := webhooks.NewWebHook("", "")
	c := NewController(wh, healthcheck.NewHandler(), m.KubeClient, m.ExtClient, m.AgonesClient, m.AgonesInformerFactory)
	c.recorder = m.FakeRecorder
	return c, m
}

func newAdmissionReview(fs v1alpha1.FleetAutoScaler) (admv1beta1.AdmissionReview, error) {
	raw, err := json.Marshal(fs)
	if err != nil {
		return admv1beta1.AdmissionReview{}, err
	}
	review := admv1beta1.AdmissionReview{
		Request: &admv1beta1.AdmissionRequest{
			Kind:      gvk,
			Operation: admv1beta1.Create,
			Object: runtime.RawExtension{
				Raw: raw,
			},
			Namespace: "default",
		},
		Response: &admv1beta1.AdmissionResponse{Allowed: true},
	}
	return review, err
}
