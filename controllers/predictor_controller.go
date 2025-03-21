// Copyright 2021 IBM Corporation
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
package controllers

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/kserve/modelmesh-serving/pkg/predictor_source"

	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kserve/modelmesh-serving/pkg/mmesh"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	kfsv1alpha1 "github.com/kserve/modelmesh-serving/apis/kfserving/v1alpha1"
	api "github.com/kserve/modelmesh-serving/apis/serving/v1alpha1"
	servingv1beta1 "github.com/kserve/modelmesh-serving/apis/serving/v1beta1"
	"github.com/kserve/modelmesh-serving/controllers/modelmesh"
	mmeshapi "github.com/kserve/modelmesh-serving/generated/mmesh"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	InferenceServiceCRSourceId = "isvc"
	PredictorCRSourceId        = "ksp"
	TrainedModelCRSourceId     = "kstm"
)

// PredictorReconciler reconciles Predictors
type PredictorReconciler struct {
	client.Client
	Log       logr.Logger
	MMService *mmesh.MMService

	RegistryLookup map[string]predictor_source.PredictorRegistry
}

// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=predictors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=predictors/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=predictors/status,verbs=get;update;patch
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=inferenceservices/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kserve.io,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kubeflow.org,resources=trainedmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kubeflow.org,resources=trainedmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:namespace=model-serving,groups=serving.kubeflow.org,resources=trainedmodels/finalizers,verbs=update
// This one is used by the kube-based grpc resolver but need to set it here so that kubebuilder picks it up
// +kubebuilder:rbac:namespace=model-serving,groups="",resources=endpoints,verbs=get;list;watch

func (pr *PredictorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nname := req.NamespacedName
	namespace := nname.Namespace
	// Check if namespace has a source prefix
	i := strings.LastIndex(namespace, "_")
	nname.Namespace = namespace[i+1:]
	source := PredictorCRSourceId // if none we default to "ksp" (for Predictor CR)
	if i > 0 {
		source = namespace[:i]
	}

	registry, ok := pr.RegistryLookup[source]
	if !ok {
		pr.Log.Error(nil, "Ignoring reconciliation event from unrecognized source",
			"namespacedName", nname, "sourceId", source)
		return ctrl.Result{}, nil
	}
	return pr.ReconcilePredictor(ctx, nname, source, registry)
}

func (pr *PredictorReconciler) ReconcilePredictor(ctx context.Context, nname types.NamespacedName,
	sourceId string, registry predictor_source.PredictorRegistry) (ctrl.Result, error) {
	resourceType := registry.GetSourceName()
	log := pr.Log.WithValues("namespacedName", nname, "source", resourceType)
	log.V(1).Info("ReconcilePredictor called")

	predictor, err := registry.Get(ctx, nname)
	if (predictor == nil && err == nil) || errors.IsNotFound(err) {
		return pr.handlePredictorNotFound(ctx, nname, sourceId)
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to fetch CR from kubebuilder cache for predictor %s: %w",
			nname.Name, err)
	}

	status := &predictor.Status
	waitingBefore := status.WaitingForRuntime()
	updateStatus := false
	mmc := pr.MMService.MMClient()
	var finalErr error
	if predictor.Spec.Storage != nil && predictor.Spec.Storage.S3 == nil {
		log.Info("Only S3 Storage currently supported", "Storage", predictor.Spec.Storage)
		if mmc != nil {
			// Don't update invalid spec but still check vmodel status to sync the existing model states
			vModelState, err := mmc.GetVModelStatus(ctx, &mmeshapi.GetVModelStatusRequest{
				VModelId: predictor.Name, Owner: sourceId,
			})
			if err != nil {
				if isNoAddresses(err) {
					mmc = nil // will mean we return retry result
				} else {
					// don't return yet because we may want to update status first
					finalErr = fmt.Errorf("failed GetVModelStatus: %w", err)
				}
			} else if pr.updatePredictorStatusFromVModel(status, vModelState, nname, false) {
				updateStatus = true
			}
		}
		// Reflect invalid spec in Status (overwrite)
		if setStatusFailureInfo(status, &api.FailureInfo{
			Reason:  api.InvalidPredictorSpec,
			Message: "Only S3 Storage is currently supported",
			ModelId: concreteModelName(predictor, sourceId),
		}) {
			updateStatus = true
		}
		if status.TransitionStatus != api.InvalidSpec {
			status.TransitionStatus = api.InvalidSpec
			updateStatus = true
		}
	} else if mmc != nil {
		// This determines whether we should trigger an explicit load of the model
		// as part of the update, e.g. if the predictor is new or transitioning
		loadNow := predictor.DeletionTimestamp == nil &&
			(status.ActiveModelState == api.Pending ||
				status.ActiveModelState == api.FailedToLoad ||
				status.TargetModelState != "" ||
				(status.ActiveModelState == api.Loading && status.WaitingForRuntime()))

		// Update vModel - idempotent
		vModelState, err := pr.setVModel(ctx, mmc, predictor, loadNow, sourceId)
		if err == nil {
			log.Info("SetVModel succeeded", "vmodelName", predictor.GetName(),
				/*"concreteModelName", concreteModelName,*/ "SetVModelResponse", vModelState)

			updateStatus = pr.updatePredictorStatusFromVModel(status, vModelState, nname, true)
		} else if isNoAddresses(err) {
			updateStatus = setStatusFailureInfo(status, &api.FailureInfo{
				Reason:  api.RuntimeUnhealthy,
				Message: "Waiting for runtime Pod to become available",
				ModelId: concreteModelName(predictor, sourceId),
			})
		} else if grpcstatus.Convert(err).Code() == codes.AlreadyExists {
			//TODO here should also extract the conflicting owner string, and also trigger a reconcile with that
			// other source id (in case it no longer exists)
			updateStatus = setStatusFailureInfo(status, &api.FailureInfo{
				Reason:  api.InvalidPredictorSpec,
				Message: "Predictor already exists with the same name from a different source",
			})
			finalErr = fmt.Errorf("failed to create vmodel %s for %s because one already exists"+
				" from different source: %w", predictor.GetName(), resourceType, err)
		} else {
			//TODO depending on kind of error we may want to update transition status to reflect
			finalErr = fmt.Errorf("failed to SetVModel for %s %s: %w", resourceType, predictor.GetName(), err)
		}
	}

	if updateStatus {
		updateStatusCtx, cancel := context.WithTimeout(ctx, K8sStatusUpdateTimeout)
		defer cancel()
		if succ, err := registry.UpdateStatus(updateStatusCtx, predictor); err != nil {
			finalErr = fmt.Errorf("unable to update Status of %s %s: %w", resourceType, predictor.GetName(), err)
		} else if !succ {
			// this can occur during normal operations
			log.Info("Unable to update " + resourceType + " Status due to resource conflict")
			return ctrl.Result{Requeue: true}, nil
		} else {
			if !waitingBefore && status.WaitingForRuntime() {
				// indicates whether model-mesh or specific runtime is unavailable
				log.Info(status.LastFailureInfo.Message)
			}
			log.Info(resourceType+" Status updated", "newStatus", *status)
		}
	}

	if finalErr != nil {
		return ctrl.Result{}, finalErr
	}

	if mmc == nil || status.WaitingForRuntime() {
		// Waiting for modelmesh client to connect or for runtime Pod to become available
		// Don't log error, just retry. With enhancements to model-mesh coming soon, we should
		// no longer need to retry in the case that some runtimes are up but not the required one
		// since it will trigger a load of the model automatically and this will result in an etcd event.
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil //TODO maybe some back-off
	}
	if status.ActiveModelState == api.Loading {
		// This is currently required since there's no explicit event in model-mesh etcd
		// corresponding to loading completion. We plan to change this but in the meantime
		// must "poll" to detect it. The same is not required for the target model state
		// because we will get a vmodel state change event when that completes.
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil //TODO maybe some back-off
	}

	return ctrl.Result{}, nil
}

// passed in ModelInfo.Key field of registration requests
type ModelKeyInfo struct {
	StorageKey *string        `json:"storage_key,omitempty"`
	Bucket     *string        `json:"bucket,omitempty"`
	ModelType  *api.ModelType `json:"model_type,omitempty"`
	SchemaPath *string        `json:"schema_path,omitempty"`
}

const (
	GrpcRequestTimeout     = 10 * time.Second
	K8sStatusUpdateTimeout = 10 * time.Second
)

var modelStateMap = map[mmeshapi.ModelStatusInfo_ModelStatus]api.ModelState{
	mmeshapi.ModelStatusInfo_NOT_LOADED:     api.Standby,
	mmeshapi.ModelStatusInfo_LOADING:        api.Loading,
	mmeshapi.ModelStatusInfo_LOADED:         api.Loaded,
	mmeshapi.ModelStatusInfo_LOADING_FAILED: api.FailedToLoad,
	//mmeshapi.ModelStatusInfo_NOT_FOUND:    api.Pending,
	//mmeshapi.ModelStatusInfo_UNKNOWN:      api.Pending,
}

var transitionStatusMap = map[mmeshapi.VModelStatusInfo_VModelStatus]api.TransitionStatus{
	mmeshapi.VModelStatusInfo_DEFINED:           api.UpToDate,
	mmeshapi.VModelStatusInfo_TRANSITIONING:     api.InProgress,
	mmeshapi.VModelStatusInfo_TRANSITION_FAILED: api.BlockedByFailedLoad,
}

func (pr *PredictorReconciler) handlePredictorNotFound(ctx context.Context,
	name types.NamespacedName, sourceId string) (ctrl.Result, error) {
	mmc := pr.MMService.MMClient()
	if mmc == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	deleteCtx, cancel := context.WithTimeout(ctx, GrpcRequestTimeout)
	defer cancel()
	_, err := mmc.DeleteVModel(deleteCtx, &mmeshapi.DeleteVModelRequest{VModelId: name.Name, Owner: sourceId})
	if err != nil {
		if isNoAddresses(err) {
			// Work-around to prevent Non-MM InferenceService indefinite reconcile loop
			// when there are no model-mesh pods running.
			if sourceId == InferenceServiceCRSourceId {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		err = fmt.Errorf("failed to remove corresponding VModel for deleted Predictor %s: %w", name, err)
		return ctrl.Result{}, err
	}
	pr.Log.Info("VModel removed", "vmodelId", name.Name, "namespace", name.Namespace)
	return ctrl.Result{}, nil
}

func (pr *PredictorReconciler) setVModel(ctx context.Context, mmc mmeshapi.ModelMeshClient,
	predictor *api.Predictor, loadNow bool, sourceId string) (*mmeshapi.VModelStatusInfo, error) {
	spec := &predictor.Spec

	setVmodelCtx, cancel := context.WithTimeout(ctx, GrpcRequestTimeout)
	defer cancel()

	mki := ModelKeyInfo{
		ModelType:  &spec.Model.Type,
		SchemaPath: spec.Model.SchemaPath,
	}

	if spec.Storage != nil && spec.Storage.S3 != nil {
		//TODO other storage types
		mki.StorageKey = &spec.Storage.S3.SecretKey
		mki.Bucket = spec.Storage.S3.Bucket
	}

	keyJSONBytes, err := json.Marshal(mki)
	if err != nil {
		return nil, fmt.Errorf("error json-marshalling VModel parameters: %w", err)
	}

	return mmc.SetVModel(setVmodelCtx, &mmeshapi.SetVModelRequest{
		VModelId:              predictor.GetName(),
		Owner:                 sourceId,
		TargetModelId:         concreteModelName(predictor, sourceId),
		AutoDeleteTargetModel: true,
		LoadNow:               loadNow,
		ModelInfo: &mmeshapi.ModelInfo{
			Type: modelmesh.GetPredictorModelTypeLabel(predictor),
			Path: spec.Path,
			Key:  string(keyJSONBytes),
		},
	})
}

// Returns the model-mesh model name corresponding to a particular Predictor and sourceId
func concreteModelName(predictor *api.Predictor, sourceId string) string {
	return fmt.Sprintf("%s__%s-%s", predictor.Name, sourceId, Hash(&predictor.Spec))
}

// This is the error message from model-mesh when there are no ready Pods which can load models of
// this model's type. Examples of the full message:
// "There are no running instances that meet the label requirements of type mt:SomeType: [mt:SomeType]"
// "There are no running instances that meet the label requirements of type rt:SomeRuntime: [rt:SomeRuntime]"
// "There are no running instances that meet the label requirements of type _default: [_no_runtime]"
const noHomeMessage string = "There are no running instances that meet the label requirements of type "

func decodeModelState(status *mmeshapi.ModelStatusInfo) (api.ModelState, api.FailureReason, string) {
	state := modelStateMap[status.Status]
	reason := api.FailureReason("")
	msg := ""
	if len(status.Errors) > 0 {
		reason, msg = api.ModelLoadFailed, status.Errors[0]
	}
	if state != api.FailedToLoad {
		return state, reason, msg
	}
	if !strings.HasPrefix(msg, noHomeMessage) {
		return api.FailedToLoad, api.ModelLoadFailed, msg
	}
	if !strings.HasSuffix(msg, "["+modelmesh.ModelTypeLabelThatNoRuntimeSupports+"]") {
		return api.Loading, api.RuntimeUnhealthy, "Waiting for supporting runtime Pod to become available"
	}
	if msg[len(noHomeMessage):len(noHomeMessage)+3] == "rt:" {
		return api.FailedToLoad, api.RuntimeNotRecognized, "Specified runtime name not recognized"
	}
	return api.FailedToLoad, api.NoSupportingRuntime, "No ServingRuntime supports specified model type"
}

// Returns true if any changes were made to the Status, false otherwise
func (pr *PredictorReconciler) updatePredictorStatusFromVModel(status *api.PredictorStatus,
	vModelState *mmeshapi.VModelStatusInfo, name types.NamespacedName, includeTransitionAndFailure bool) (changed bool) {
	ts := transitionStatusMap[vModelState.Status]
	ams, amfr, amm := decodeModelState(vModelState.ActiveModelStatus)
	if ams == "" {
		pr.Log.Error(nil, "Unexpected Model State returned from SetVModel",
			"namespacedName", name, "Status", vModelState.ActiveModelStatus.Status)
	} else if status.ActiveModelState != ams {
		status.ActiveModelState = ams
		changed = true
	}

	tmsBefore := status.TargetModelState
	counts := [3]int{}
	if amfr == "" || amfr == api.ModelLoadFailed {
		countModelCopyStates(vModelState.ActiveModelStatus.ModelCopyInfos, &counts)
	}
	var targetModelStatus *mmeshapi.ModelStatusInfo
	var targetModelFailureReason api.FailureReason
	var targetModelMessage string
	if vModelState.ActiveModelId == vModelState.TargetModelId {
		targetModelStatus = vModelState.ActiveModelStatus
		targetModelFailureReason = amfr
		targetModelMessage = amm
		// Only show a separate target model state if the target model
		// is not the same as the active model
		status.TargetModelState = ""
	} else {
		targetModelStatus = vModelState.TargetModelStatus
		if targetModelStatus != nil {
			status.TargetModelState, targetModelFailureReason, targetModelMessage = decodeModelState(targetModelStatus)
			// Ignore returned ModelCopyInfos in cases where there can't be any copies (due to model-mesh "bug"
			// where a ModelCopyInfo can be returned with non-copy related failure information)
			if targetModelFailureReason == "" || targetModelFailureReason == api.ModelLoadFailed {
				countModelCopyStates(targetModelStatus.ModelCopyInfos, &counts)
			}
		} else {
			pr.Log.Error(nil, "No TargetModelStatus returned from SetVModel",
				"namespacedName", name, "Status", vModelState.Status)
		}
	}
	if status.TargetModelState != tmsBefore {
		changed = true
	}

	if includeTransitionAndFailure {
		if ts == api.BlockedByFailedLoad && targetModelStatus != nil &&
			targetModelStatus.Status == mmeshapi.ModelStatusInfo_LOADING {
			// This is for the case where we have converted the "nowhere to load model of this type"
			// failure back to Loading
			ts = api.InProgress
		}
		if status.TransitionStatus != ts {
			status.TransitionStatus = ts
			changed = true
		}
		var fi *api.FailureInfo = nil
		if targetModelStatus != nil && targetModelStatus.Status != mmeshapi.ModelStatusInfo_LOADED {
			if targetModelFailureReason == "" {
				if status.WaitingForRuntime() {
					// Retain last failure info if we are back in a Loading state (e.g. retry loading)
					fi = status.LastFailureInfo
				}
			} else {
				fi = &api.FailureInfo{
					Reason:  targetModelFailureReason,
					ModelId: vModelState.TargetModelId,
					Message: targetModelMessage,
				}
				// Only fill in location if it's applicable to the the failure reason
				if targetModelFailureReason == api.ModelLoadFailed {
					for _, info := range targetModelStatus.ModelCopyInfos {
						if info != nil && info.CopyStatus == mmeshapi.ModelStatusInfo_LOADING_FAILED {
							fi.Location = info.Location
							if info.Time != 0 {
								// convert ms to s and ns
								failTime := v1.Unix(int64(info.Time/1000), int64((info.Time%1000)*1000000))
								fi.Time = &failTime
							}
							break
						}
					}
				} else if now, lfi := v1.Now(), status.LastFailureInfo; lfi == nil || lfi.Time == nil ||
					now.Sub(lfi.Time.Time) > 20*time.Second {
					// Use current time for other failure reasons (related to current state rather than
					// specific prior failure event)
					fi.Time = &now
				} else {
					// Do not update the time if we are within 20 sec of the last time - this will
					// avoid tight reconciliation loops where each status update triggers another one
					fi.Time = lfi.Time
				}
			}
		}

		if setStatusFailureInfo(status, fi) {
			changed = true
		}
	}

	status.Available = status.ActiveModelState != "" &&
		status.ActiveModelState != api.FailedToLoad && !status.WaitingForRuntime()
	status.GrpcEndpoint = fmt.Sprintf("grpc://%s", pr.MMService.InferenceEndpoint())
	status.HTTPEndpoint = pr.MMService.InferenceRESTEndpoint()

	// This will be reinstated once the loading/loaded counts are added back to the Predictor CRD Status
	//if counts != [3]int{status.LoadingCopies, status.LoadedCopies, status.FailedCopies} {
	//	status.LoadingCopies, status.LoadedCopies, status.FailedCopies = counts[0], counts[1], counts[2]
	//	changed = true
	//}

	if counts[2] != status.FailedCopies {
		status.FailedCopies = counts[2]
		changed = true
	}

	return
}

// returns true if changed
func setStatusFailureInfo(crStatus *api.PredictorStatus, info *api.FailureInfo) bool {
	if reflect.DeepEqual(info, crStatus.LastFailureInfo) {
		return false
	}
	crStatus.LastFailureInfo = info
	return true
}

func countModelCopyStates(copyInfos []*mmeshapi.ModelStatusInfo_ModelCopyInfo, counts *[3]int) {
	for _, info := range copyInfos {
		if info != nil {
			switch info.CopyStatus {
			case mmeshapi.ModelStatusInfo_LOADING:
				counts[0] += 1
			case mmeshapi.ModelStatusInfo_LOADED:
				counts[1] += 1
			case mmeshapi.ModelStatusInfo_LOADING_FAILED:
				counts[2] += 1
			}
		}
	}
}

func isNoAddresses(err error) bool {
	s := grpcstatus.Convert(err)
	return s.Code() == codes.Unavailable && strings.Contains(s.Message(), "produced zero addresses")
}

// Hash returns a 10-character hash string of the spec
func Hash(predictorSpec *api.PredictorSpec) string {
	b, _ := json.Marshal(predictorSpec) //TODO check for things to exclude
	hsha1 := sha1.Sum(b)
	return hex.EncodeToString(hsha1[:5])
}

// ---------

func (pr *PredictorReconciler) SetupWithManager(mgr ctrl.Manager, eventStream *mmesh.ModelMeshEventStream,
	watchTrainedModels bool, watchInferenceServices bool, sourcePluginEvents <-chan event.GenericEvent) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&api.Predictor{}).
		Watches(&source.Channel{Source: eventStream.MMEvents}, &handler.EnqueueRequestForObject{})

	if sourcePluginEvents != nil {
		builder.Watches(&source.Channel{Source: sourcePluginEvents}, &handler.EnqueueRequestForObject{})
	}

	if watchTrainedModels {
		builder = builder.Watches(&source.Kind{Type: &kfsv1alpha1.TrainedModel{}}, prefixName(TrainedModelCRSourceId))
	}
	if watchInferenceServices {
		builder = builder.Watches(&source.Kind{Type: &servingv1beta1.InferenceService{}}, prefixName(InferenceServiceCRSourceId))
	}
	return builder.Complete(pr)
}

func prefixName(prefix string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		// Prepend prefix
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Namespace: fmt.Sprintf("%s_%s", prefix, o.GetNamespace()),
				Name:      o.GetName(),
			}},
		}
	})
}
