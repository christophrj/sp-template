package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DomainServiceReconciler implements any business logic required to manage your APIObject
type DomainServiceReconciler[T APIObject, PC ProviderConfig] interface {
	// CreateOrUpdate is called on every add or update event
	CreateOrUpdate(ctx context.Context, obj T, pc PC, target *clusters.Cluster) (ctrl.Result, error)
	// Delete is called on every delete event
	Delete(ctx context.Context, obj T, pc PC, target *clusters.Cluster) (ctrl.Result, error)
}

// APIObject represents an onboarding api type
type APIObject interface {
	client.Object
	APIObjectStatus
	Finalizer() string
}

// APIObjectStatus represents the status type of an onboarding api type
type APIObjectStatus interface {
	// GetStatus returns the status object
	GetStatus() any
	// GetConditions returns the status object
	GetConditions() *[]metav1.Condition
	// SetPhase sets Status.Phase
	SetPhase(string)
	// SetObservedGeneration sets Status.ObservedGeneration
	SetObservedGeneration(int64)
}

// ProviderConfig represents the config for platform operators
// The ProviderConfig is passed to the DomainServiceReconcile to reconcile APIObjects
type ProviderConfig interface {
	client.Object
	// PollIntveral can be used to periodically requeue, preventing managed objects
	// from drifting on the target cluster.  Return 0 if not required.
	PollInterval() time.Duration
}

// SPReconciler implements a generic reconcile loop to separate platform
// and service provider developer space.
type SPReconciler[T APIObject, PC ProviderConfig] struct {
	PlatformCluster         *clusters.Cluster
	OnboardingCluster       *clusters.Cluster
	ClusterAccessReconciler clusteraccess.Reconciler
	DomainServiceReconciler DomainServiceReconciler[T, PC]
	EmptyAPIObj             func() T
	ProviderConfig          atomic.Pointer[PC]
}

// Reconcile orchestrates platform and DomainServiceReconciler logic to reconcile APIObjects
func (r *SPReconciler[T, PC]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// common reconciler logic including get obj, providerconfig, mcp/workload access
	obj := r.EmptyAPIObj()
	if err := r.OnboardingCluster.Client().Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	oldObj := obj.DeepCopyObject().(T)
	providerConfig := r.ProviderConfig.Load()
	if providerConfig == nil {
		StatusProgressing(obj, "ReconcileError", "No ProviderConfig found")
		r.updateStatus(ctx, obj, oldObj)
		return ctrl.Result{}, errors.New("provider config missing")
	}
	providerConfigCopy := (*providerConfig).DeepCopyObject().(PC)
	// TODO workload cluster access
	mcp, res, err := r.mcp(ctx, req)
	if err != nil {
		l.Error(err, "cluster access error")
		StatusProgressing(obj, "ReconcileError", "target cluster access error")
		r.updateStatus(ctx, obj, oldObj)
		return ctrl.Result{}, err
	}
	if mcp == nil {
		StatusProgressing(obj, "Reconciling", "target cluster access is being set up")
		r.updateStatus(ctx, obj, oldObj)
		return res, nil
	}
	// core crud
	deleted := !obj.GetDeletionTimestamp().IsZero()
	if deleted {
		res, err = r.delete(ctx, obj, providerConfigCopy, mcp)
	} else {
		res, err = r.createOrUpdate(ctx, obj, providerConfigCopy, mcp)
		r.updateStatus(ctx, obj, oldObj)
	}
	// return based on result/err
	if err != nil {
		l.Error(err, "reconcile failed")
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}
	// fallback to poll interval to prevent 'managed service' drift
	return ctrl.Result{
		RequeueAfter: providerConfigCopy.PollInterval(),
	}, nil
}

func (r *SPReconciler[T, PC]) updateStatus(ctx context.Context, new T, old T) {
	if equality.Semantic.DeepEqual(old.GetStatus(), new.GetStatus()) {
		return
	}
	if err := r.OnboardingCluster.Client().Status().Patch(ctx, new, client.MergeFrom(old)); err != nil {
		l := logf.FromContext(ctx)
		l.Error(err, "Patch status failed")
	}
}

func (r *SPReconciler[T, PC]) mcp(ctx context.Context, req ctrl.Request) (*clusters.Cluster, ctrl.Result, error) {
	res, err := r.ClusterAccessReconciler.Reconcile(ctx, req)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return nil, res, nil
	}
	mcpCluster, err := r.ClusterAccessReconciler.MCPCluster(ctx, req)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	return mcpCluster, ctrl.Result{}, nil
}

func (r *SPReconciler[T, PC]) delete(ctx context.Context, obj T, pc PC, mcp *clusters.Cluster) (ctrl.Result, error) {
	oldObj := obj.DeepCopyObject().(T)
	res, err := r.DomainServiceReconciler.Delete(ctx, obj, pc, mcp)
	r.updateStatus(ctx, obj, oldObj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}
	// remove mcp access
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	res, err = r.ClusterAccessReconciler.ReconcileDelete(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	// make sure to not drop the object before cleanup has been done
	if res.RequeueAfter > 0 {
		return res, nil
	}
	// remove finalizer
	controllerutil.RemoveFinalizer(obj, obj.Finalizer())
	if err := r.OnboardingCluster.Client().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
func (r *SPReconciler[T, PC]) createOrUpdate(
	ctx context.Context, obj T, pc PC, mcp *clusters.Cluster,
) (ctrl.Result, error) {
	controllerutil.AddFinalizer(obj, obj.Finalizer())
	if err := r.OnboardingCluster.Client().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return r.DomainServiceReconciler.CreateOrUpdate(ctx, obj, pc, mcp)
}
