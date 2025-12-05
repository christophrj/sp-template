package runtime

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ServiceProviderConditionSynced = "Synced"
	ServiceProviderConditionReady  = "Ready"
	// StatusPhaseReady indicates that the resource is ready. All conditions are met and are in status "True".
	StatusPhaseReady = "Ready"
	// StatusPhaseProgressing indicates that the resource is not ready and being created or updated.
	// At least one condition is not met and is in status "False".
	StatusPhaseProgressing = "Progressing"
	// StatusPhaseTerminating indicates that the resource is not ready and in deletion.
	// At least one condition is not met and is in status "False".
	StatusPhaseTerminating = "Terminating"
)

// indicates progressing with synced false
func StatusProgressing(obj ApiObject, reason string, message string) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             reason,
		Message:            message,
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	obj.SetPhase(StatusPhaseProgressing)
}

// indicates progressing with synced true
func StatusSynced(obj ApiObject) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionSynced,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             "ServiceSynced",
		Message:            "Domain Service is synced",
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	obj.SetPhase(StatusPhaseProgressing)
}

// indicates errors through synced and ready false but does not result in a phase transition
func StatusError(obj ApiObject, reason string, message string) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             reason,
		Message:            message,
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	// set ready false
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             "ServiceNotReady",
		Message:            "Errors detected",
	})
}

// indicates ready with ready true
func StatusReady(obj ApiObject) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             "ServiceReady",
		Message:            "Domain Service is ready",
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	obj.SetPhase(StatusPhaseReady)
}

// indicates terminating with synced false
func StatusTerminating(obj ApiObject) {
	meta.SetStatusCondition(obj.GetConditions(), metav1.Condition{
		Type:               ServiceProviderConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             "Terminating",
		Message:            "Cleanup in progress",
	})
	obj.SetObservedGeneration(obj.GetGeneration())
	obj.SetPhase(StatusPhaseTerminating)
}
