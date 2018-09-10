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
	"time"

	"agones.dev/agones/pkg/apis/stable"
	stablev1alpha1 "agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/mattbaird/jsonpatch"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// Controller is a the FleetAutoScaler controller
type Controller struct {
	logger                *logrus.Entry
	crdGetter             v1beta1.CustomResourceDefinitionInterface
	fleetGetter           getterv1alpha1.FleetsGetter
	fleetLister           listerv1alpha1.FleetLister
	fleetAutoScalerGetter getterv1alpha1.FleetAutoScalersGetter
	fleetAutoScalerLister listerv1alpha1.FleetAutoScalerLister
	fleetAutoScalerSynced cache.InformerSynced
	workerqueue           *workerqueue.WorkerQueue
	recorder              record.EventRecorder
}

// NewController returns a controller for a FleetAutoScaler
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	kubeClient kubernetes.Interface,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	agonesInformer := agonesInformerFactory.Stable().V1alpha1()
	fasInformer := agonesInformer.FleetAutoScalers().Informer()

	c := &Controller{
		crdGetter:             extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		fleetGetter:           agonesClient.StableV1alpha1(),
		fleetLister:           agonesInformer.Fleets().Lister(),
		fleetAutoScalerGetter: agonesClient.StableV1alpha1(),
		fleetAutoScalerLister: agonesInformer.FleetAutoScalers().Lister(),
		fleetAutoScalerSynced: fasInformer.HasSynced,
	}
	c.logger = runtime.NewLoggerWithType(c)
	c.workerqueue = workerqueue.NewWorkerQueue(c.syncFleetAutoScaler, c.logger, stable.GroupName+".FleetAutoScalerController")
	health.AddLivenessCheck("fleetautoscaler-workerqueue", healthcheck.Check(c.workerqueue.Healthy))

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "fleetautoscaler-controller"})

	kind := stablev1alpha1.Kind("FleetAutoScaler")
	wh.AddHandler("/mutate", kind, admv1beta1.Create, c.creationMutationHandler)
	wh.AddHandler("/validate", kind, admv1beta1.Update, c.mutationValidationHandler)

	fasInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.workerqueue.Enqueue,
		UpdateFunc: func(_, newObj interface{}) {
			c.workerqueue.Enqueue(newObj)
		},
	})

	return c
}

// Run runs this controller. This controller doesn't (currently)
// have a worker/queue, and as such, does not block.
func (c *Controller) Run(workers int, stop <-chan struct{}) error {
	err := crd.WaitForEstablishedCRD(c.crdGetter, "fleetautoscalers."+stable.GroupName, c.logger)
	if err != nil {
		return err
	}

	c.logger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.fleetAutoScalerSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	c.workerqueue.Run(workers, stop)
	return nil
}

// creationMutationHandler will intercept when a FleetAutoScaler is created, and attach it to the requested Fleet
// assuming that it exists. If not, it will reject the AdmissionReview.
func (c *Controller) creationMutationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("creationMutationHandler")
	obj := review.Request.Object
	fas := &stablev1alpha1.FleetAutoScaler{}
	err := json.Unmarshal(obj.Raw, fas)
	if err != nil {
		return review, errors.Wrapf(err, "error unmarshalling original FleetAutoScaler json: %s", obj.Raw)
	}

	var causes []metav1.StatusCause
	causes = fas.ValidateAutoScalingSettings(causes)
	if len(causes) != 0 {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "FleetAutoScaler creation is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}
		return review, nil
	}

	// When being called from the API the fas.ObjectMeta.Namespace isn't populated
	// (whereas it is from kubectl). So make sure to pull the namespace from the review
	fleet, err := c.fleetLister.Fleets(review.Request.Namespace).Get(fas.Spec.FleetName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			review.Response.Allowed = false
			review.Response.Result = &metav1.Status{
				Status:  metav1.StatusFailure,
				Reason:  metav1.StatusReasonInvalid,
				Message: "Invalid FleetAutoScaler",
				Details: &metav1.StatusDetails{
					Name:  review.Request.Name,
					Group: review.Request.Kind.Group,
					Kind:  review.Request.Kind.Kind,
					Causes: []metav1.StatusCause{
						{Type: metav1.CauseTypeFieldValueNotFound,
							Message: fmt.Sprintf("Could not find fleet %s in namespace %s", fas.Spec.FleetName, review.Request.Namespace),
							Field:   "fleetName"}},
				},
			}
			return review, nil
		}
		return review, errors.Wrapf(err, "error retrieving fleet %s", fas.Name)
	}

	// Attach to the fleet
	// When a Fleet is deleted, the FleetAutoScaler should go with it
	ref := metav1.NewControllerRef(fleet, stablev1alpha1.SchemeGroupVersion.WithKind("Fleet"))
	fas.ObjectMeta.OwnerReferences = append(fas.ObjectMeta.OwnerReferences, *ref)

	// Q: can we do this nicer?
	c.recorder.Eventf(fleet, corev1.EventTypeNormal, string("CreatingFleetAutoScaler"), "Attached FleetAutoScaler %s to fleet %s", fas.ObjectMeta.Name, fas.Spec.FleetName)
	c.recorder.Eventf(fas, corev1.EventTypeNormal, string("CreatingFleetAutoScaler"), "Attached FleetAutoScaler %s to fleet %s", fas.ObjectMeta.Name, fas.Spec.FleetName)

	newFS, err := json.Marshal(fas)
	if err != nil {
		return review, errors.Wrapf(err, "error marshalling FleetAutoScaler %s to json", fas.ObjectMeta.Name)
	}

	patch, err := jsonpatch.CreatePatch(obj.Raw, newFS)
	if err != nil {
		return review, errors.Wrapf(err, "error creating patch for FleetAutoScaler %s", fas.ObjectMeta.Name)
	}

	json, err := json.Marshal(patch)
	if err != nil {
		return review, errors.Wrapf(err, "error creating json for patch for FleetAutoScaler %s", fas.ObjectMeta.Name)
	}

	c.logger.WithField("fas", fas.ObjectMeta.Name).WithField("patch", string(json)).Infof("patch created!")

	pt := admv1beta1.PatchTypeJSONPatch
	review.Response.PatchType = &pt
	review.Response.Patch = json

	return review, nil
}

// mutationValidationHandler stops edits from happening to a
// FleetAutoScaler fleetName value
func (c *Controller) mutationValidationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("mutationValidationHandler")

	newFS := &stablev1alpha1.FleetAutoScaler{}
	oldFS := &stablev1alpha1.FleetAutoScaler{}

	if err := json.Unmarshal(review.Request.Object.Raw, newFS); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling new FleetAutoScaler json: %s", review.Request.Object.Raw)
	}

	if err := json.Unmarshal(review.Request.OldObject.Raw, oldFS); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling old FleetAutoScaler json: %s", review.Request.Object.Raw)
	}

	causes := oldFS.ValidateUpdate(newFS, nil)
	if len(causes) != 0 {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "FleetAutoScaler update is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}
	}

	return review, nil
}

// syncFleetAutoScaler needs doc
func (c *Controller) syncFleetAutoScaler(key string) error {
	c.logger.WithField("key", key).Info("Synchronising")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.logger.WithField("key", key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	fas, err := c.fleetAutoScalerLister.FleetAutoScalers(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("key", key).Info(fmt.Sprintf("FleetAutoScaler %s from namespace %s is no longer available for syncing", name, namespace))
			return nil
		}
		return errors.Wrapf(err, "error retrieving FleetAutoScaler %s from namespace %s", name, namespace)
	}

	// Retrieve the fleet by spec name
	fleet, err := c.fleetLister.Fleets(namespace).Get(fas.Spec.FleetName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logrus.WithError(err).WithField("fleetAutoScalerName", fas.Name).
				WithField("fleetName", fas.Spec.FleetName).
				WithField("namespace", namespace).
				Warn("Could not find fleet for autoscaler. Skipping.")
			return errors.Wrapf(err, "fleet %s not found in namespace %s", fas.Spec.FleetName, namespace)
		}
		c.updateStatusUnableToScale(fas)
		return errors.Wrapf(err, "error retrieving fleet %s from namespace %s", fas.Spec.FleetName, namespace)
	}

	currentReplicas := fleet.Status.Replicas
	desiredReplicas, scalingLimited := computeDesiredFleetSize(fas, fleet)

	// Scale the fleet to the new size
	err = c.scaleFleet(fas, fleet, desiredReplicas)
	if err != nil {
		return errors.Wrapf(err, "error autoscaling fleet %s to %d replicas", fas.Spec.FleetName, desiredReplicas)
	}

	return c.updateStatus(fas, currentReplicas, desiredReplicas, desiredReplicas != fleet.Spec.Replicas, scalingLimited)
}

// scaleFleet scales the fleet of the autoscaler to a new number of replicas
func (c *Controller) scaleFleet(fas *stablev1alpha1.FleetAutoScaler, f *stablev1alpha1.Fleet, replicas int32) error {
	if replicas != f.Spec.Replicas {
		fCopy := f.DeepCopy()
		fCopy.Spec.Replicas = replicas
		fCopy, err := c.fleetGetter.Fleets(f.ObjectMeta.Namespace).Update(fCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating replicas for fleet %s", f.ObjectMeta.Name)
		}

		c.recorder.Eventf(fas, corev1.EventTypeNormal, "AutoScalingFleet",
			"Scaling fleet %s from %d to %d", fCopy.ObjectMeta.Name, f.Spec.Replicas, fCopy.Spec.Replicas)
	}

	return nil
}

// updateStatus updates the status of the given FleetAutoScaler
func (c *Controller) updateStatus(fas *stablev1alpha1.FleetAutoScaler, currentReplicas int32, desiredReplicas int32, scaled bool, scalingLimited bool) error {
	// Q: can we detect and update only when fleet data is changed?
	fasCopy := fas.DeepCopy()
	fasCopy.Status.AbleToScale = true
	fasCopy.Status.ScalingLimited = scalingLimited
	fasCopy.Status.CurrentReplicas = currentReplicas
	fasCopy.Status.DesiredReplicas = desiredReplicas
	if scaled {
		now := metav1.NewTime(time.Now())
		fasCopy.Status.LastScaleTime = &now
	}

	if !apiequality.Semantic.DeepEqual(fas.Status, fasCopy.Status) {
		_, err := c.fleetAutoScalerGetter.FleetAutoScalers(fas.ObjectMeta.Namespace).Update(fasCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status for fleetautoscaler %s", fas.ObjectMeta.Name)
		}
	}

	return nil
}

// updateStatus updates the status of the given FleetAutoScaler in the case we're not able to scale
func (c *Controller) updateStatusUnableToScale(fas *stablev1alpha1.FleetAutoScaler) error {
	// Q: can we detect and update only when fleet data is changed?
	fasCopy := fas.DeepCopy()
	fasCopy.Status.AbleToScale = false
	fasCopy.Status.ScalingLimited = false
	fasCopy.Status.CurrentReplicas = 0
	fasCopy.Status.DesiredReplicas = 0

	if !apiequality.Semantic.DeepEqual(fas.Status, fasCopy.Status) {
		_, err := c.fleetAutoScalerGetter.FleetAutoScalers(fas.ObjectMeta.Namespace).Update(fasCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status for fleetautoscaler %s", fas.ObjectMeta.Name)
		}
	}

	return nil
}
