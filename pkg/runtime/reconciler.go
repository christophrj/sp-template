package runtime

import (
	"context"
	"time"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type DomainServiceReconciler[T ApiObject, PC ProviderConfig] interface {
	CreateOrUpdate(ctx context.Context, obj T, pc PC, mcp client.Client) (ctrl.Result, error)
	Delete(ctx context.Context, obj T, pc PC, mcp client.Client) (ctrl.Result, error)
}

type ApiObject interface {
	client.Object
	ApiObjectStatus
	MyDeepCopy() any
	Deleted() bool
	Finalizer() string
}

type ApiObjectStatus interface {
	GetStatus() any
	GetConditions() *[]metav1.Condition
	SetPhase(string)
	SetObservedGeneration(int64)
}

type ProviderConfig interface {
	client.Object
	PollInterval() time.Duration
}

type Reconciler[T ApiObject, PC ProviderConfig] struct {
	PlatformCluster         *clusters.Cluster
	ClusterAccessReconciler clusteraccess.Reconciler
	// onboarding client
	client.Client
	Scheme                  *runtime.Scheme
	DomainServiceReconciler DomainServiceReconciler[T, PC]
	EmptyAPIObj             func() T
	EmptyProviderConfig     func() PC
}

func (r *Reconciler[T, PC]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// core logic
	svcObj := r.EmptyAPIObj()
	if err := r.Get(ctx, req.NamespacedName, svcObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// track changes
	old := svcObj.MyDeepCopy().(T)
	StatusProgressing(svcObj, "Reconciling", "ServiceProvider is reconciling")
	// resolve provider config
	pc := r.EmptyProviderConfig()
	if err := r.PlatformCluster.Client().Get(ctx, types.NamespacedName{Name: "default"}, pc); err != nil {
		l.Error(err, "ProviderConfig missing")
		StatusError(svcObj, "ProviderConfigMissing", "No ProviderConfig found")
		r.updateStatus(ctx, svcObj, old)
		return ctrl.Result{}, err
	}
	// resolve mcp
	mcp, res, err := r.mcpCluster(ctx, req)
	// var err error
	if err != nil {
		l.Error(err, "MCP access failure")
		StatusError(svcObj, "MCPAccessFailure", err.Error())
		r.updateStatus(ctx, svcObj, old)
		return ctrl.Result{}, err
	}
	// mcp access not ready
	// var res ctrl.Result
	// var mcp *clusters.Cluster
	if mcp == nil {
		StatusProgressing(svcObj, "MCPAccessSetup", "MCP access is being set up")
		r.updateStatus(ctx, svcObj, old)
		return res, nil
	}

	// check annotations to e.g. end reconcile
	// TODO
	// end check annotations

	// core crud
	deleted := svcObj.Deleted()
	if deleted {
		res, err = r.delete(ctx, svcObj, pc, mcp)
	} else {
		res, err = r.createOrUpdate(ctx, svcObj, pc, mcp)
	}
	// return based on result/err
	if err != nil {
		l.Error(err, "reconcile failed")
		r.updateStatus(ctx, svcObj, old)
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		r.updateStatus(ctx, svcObj, old)
		return res, nil
	}
	// no error, delete or requeue implies a ready service
	if !deleted {
		StatusReady(svcObj)
		r.updateStatus(ctx, svcObj, old)
	}
	// fallback to poll interval to prevent 'managed service' drift
	return ctrl.Result{
		RequeueAfter: pc.PollInterval(),
	}, nil
}

func (r *Reconciler[T, PC]) updateStatus(ctx context.Context, new T, old T) {
	if equality.Semantic.DeepEqual(old.GetStatus(), new.GetStatus()) {
		return
	}
	if err := r.Status().Patch(ctx, new, client.MergeFrom(old)); err != nil {
		l := logf.FromContext(ctx)
		l.Error(err, "Patch status failed")
	}
}

func (r *Reconciler[T, PC]) mcpCluster(ctx context.Context, req ctrl.Request) (*clusters.Cluster, ctrl.Result, error) {
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

func (r *Reconciler[T, PC]) delete(ctx context.Context, obj T, providerConfig PC, mcp *clusters.Cluster) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	StatusTerminating(obj)
	// render managed objs
	res, err := r.DomainServiceReconciler.Delete(ctx, obj, providerConfig, mcp.Client())
	if err != nil {
		l.Error(err, "domain delete failed")
		StatusError(obj, "DeleteFailed", err.Error())
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}
	// remove mcp access
	res, err = r.ClusterAccessReconciler.ReconcileDelete(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	if err != nil {
		l.Error(err, "mcp access failure")
		StatusError(obj, "MCPAccessFailed", err.Error())
		return ctrl.Result{}, err
	}
	// make sure to not drop the object before cleanup has been done
	if res.RequeueAfter > 0 {
		return res, nil
	}
	// remove finalizer
	controllerutil.RemoveFinalizer(obj, obj.Finalizer())
	if err := r.Update(ctx, obj); err != nil {
		l.Error(err, "update failed")
		StatusError(obj, "UpdateFailed", err.Error())
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
func (r *Reconciler[T, PC]) createOrUpdate(ctx context.Context, svcobj T, provicerConfig PC, mcp *clusters.Cluster) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// add finalizer
	controllerutil.AddFinalizer(svcobj, svcobj.Finalizer())
	if err := r.Update(ctx, svcobj); err != nil {
		l.Error(err, "update failed")
		StatusError(svcobj, "UpdateFailed", err.Error())
		return ctrl.Result{}, err
	}
	// render managed objs
	res, err := r.DomainServiceReconciler.CreateOrUpdate(ctx, svcobj, provicerConfig, mcp.Client())
	if err != nil {
		l.Error(err, "CreateOrUpdate failed")
		StatusError(svcobj, "UpdateFailed", err.Error())
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}
	StatusSynced(svcobj)
	return ctrl.Result{}, nil
}
