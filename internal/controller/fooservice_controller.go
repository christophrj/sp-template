/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"embed"
	"io/fs"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
)

const (
	ownerNameLabel      = "foo-service/owner-name"
	ownerNamespaceLabel = "foo-service/owner-namespace"
)

var (
	finalizer = apiv1alpha1.GroupVersion.Group + "/finalizer"
)

// FooServiceReconciler reconciles a FooService object
type FooServiceReconciler struct {
	PlatformCluster         *clusters.Cluster
	ClusterAccessReconciler clusteraccess.Reconciler
	// onboarding client
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=services.openmcp.cloud,resources=fooservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.openmcp.cloud,resources=fooservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.openmcp.cloud,resources=fooservices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the FooService object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *FooServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	l.Info("reconcile...")
	// core logic
	var svcObj apiv1alpha1.FooService
	if err := r.Get(ctx, req.NamespacedName, &svcObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var mcp client.Client
	res, err := r.mcpClient(ctx, req, &mcp)
	if err != nil {
		return ctrl.Result{}, err
	}
	// mcp access not ready
	if mcp == nil {
		return res, nil
	}
	// check annotations to e.g. end reconcile
	// end check annotations
	if svcObj.DeletionTimestamp.IsZero() {
		return r.createOrUpdate(ctx, svcObj, mcp)
	}
	res, err = r.delete(ctx, svcObj, mcp)
	if err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *FooServiceReconciler) delete(ctx context.Context, obj apiv1alpha1.FooService, mcp client.Client) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// render managed objs
	objs, err := renderKustomize()
	if err != nil {
		return ctrl.Result{}, err
	}
	// remove managed objs
	for _, obj := range objs {
		if err := mcp.Delete(ctx, obj); err != nil {
			l.Error(err, "delete failed")
		}
		continue
	}
	// remove mcp access
	res, err := r.ClusterAccessReconciler.ReconcileDelete(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.Namespace,
			Name:      obj.Name,
		},
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	// make sure to not drop the object before cleanup has been done
	if res.RequeueAfter > 0 {
		return res, err
	}
	// remove finalizer
	controllerutil.RemoveFinalizer(&obj, finalizer)
	if err := r.Update(ctx, &obj); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("delete successful")
	return ctrl.Result{}, nil
}

func (r *FooServiceReconciler) createOrUpdate(ctx context.Context, svcobj apiv1alpha1.FooService, mcp client.Client) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// add finalizer
	controllerutil.AddFinalizer(&svcobj, finalizer)
	if err := r.Update(ctx, &svcobj); err != nil {
		return ctrl.Result{}, err
	}
	// render managed objs
	objs, err := renderKustomize()
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, obj := range objs {
		// set ownership labels
		labels := map[string]string{}
		labels[ownerNameLabel] = svcobj.Name
		labels[ownerNamespaceLabel] = svcobj.Namespace
		obj.SetLabels(labels)
		// prepare createOrUpdate
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(obj.GroupVersionKind())
		current.SetName(obj.GetName())
		current.SetNamespace(obj.GetNamespace())
		_, err = controllerutil.CreateOrUpdate(ctx, mcp, current, func() error {
			ignore := map[string]bool{
				"status":   true,
				"metadata": true,
			}
			for k, v := range obj.Object {
				if ignore[k] {
					continue
				}
				current.Object[k] = v
			}
			current.SetLabels(obj.GetLabels())
			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	l.Info("createOrUpdate successful")
	return ctrl.Result{}, nil
}

func (r *FooServiceReconciler) mcpClient(ctx context.Context, req ctrl.Request, mcpClient *client.Client) (ctrl.Result, error) {
	res, err := r.ClusterAccessReconciler.Reconcile(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}
	mcpCluster, err := r.ClusterAccessReconciler.MCPCluster(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	*mcpClient = mcpCluster.Client()
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FooServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.FooService{}).
		// sample watch to prevent drift on operator deployment
		// TODO 'move' to mcp since deployment is not provisioned on the onboarding cluster
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(ownershipFilter()),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Named("fooservice").
		Complete(r)
}

func ownershipFilter() handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		owner := labels[ownerNameLabel]
		if owner == "" {
			return nil
		}
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: labels[ownerNamespaceLabel],
					Name:      owner,
				},
			},
		}
	}
}

//go:embed config
var operatorKustomizeFS embed.FS

func renderKustomize() ([]*unstructured.Unstructured, error) {
	kustomizeFS := filesys.MakeFsInMemory()
	if err := fs.WalkDir(operatorKustomizeFS, "config", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		bytes, err := operatorKustomizeFS.ReadFile(path)
		if err != nil {
			return err
		}
		return kustomizeFS.WriteFile(path, bytes)
	}); err != nil {
		return nil, err
	}
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(kustomizeFS, "config/default")
	if err != nil {
		return nil, err
	}
	yamlBytes, err := rm.AsYaml()
	if err != nil {
		return nil, err
	}
	// from flux kustomize controller
	objs := []*unstructured.Unstructured{}
	manifests := strings.Split(string(yamlBytes), "\n---\n")
	for _, manifest := range manifests {
		if strings.TrimSpace(manifest) == "" {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(manifest), u); err != nil {
			return nil, err
		}
		objs = append(objs, u)
	}
	// make sure namespace gets created first
	slices.SortFunc(objs, func(a *unstructured.Unstructured, b *unstructured.Unstructured) int {
		if a.GetKind() == "Namespace" {
			return -1
		}
		return 1
	})
	return objs, nil
}
