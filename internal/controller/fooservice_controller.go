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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/openmcp-operator/lib/clusteraccess"
	apiv1alpha1 "github.com/openmcp-project/service-provider-template/api/v1alpha1"
	spruntime "github.com/openmcp-project/service-provider-template/pkg/runtime"
)

const (
	ownerNameLabel      = "foo-service/owner-name"
	ownerNamespaceLabel = "foo-service/owner-namespace"
)

// FooServiceReconciler reconciles a FooService object
type FooServiceReconciler struct {
	PlatformCluster         *clusters.Cluster
	ClusterAccessReconciler clusteraccess.Reconciler
	// onboarding client
	client.Client
	Scheme *runtime.Scheme
}

func (r *FooServiceReconciler) CreateOrUpdate(ctx context.Context, svcobj *apiv1alpha1.FooService, providerConfig *apiv1alpha1.ProviderConfig, mcp client.Client) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	// render managed objs
	objs, err := renderKustomize()
	if err != nil {
		l.Error(err, "templating failed")
		spruntime.StatusError(svcobj, "TemplatingFailed", err.Error())
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
			l.Error(err, "CreateOrUpdate failed")
			spruntime.StatusError(svcobj, "CreateOrUpdateFailed", err.Error())
			return ctrl.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *FooServiceReconciler) Delete(ctx context.Context, obj *apiv1alpha1.FooService, providerConfig *apiv1alpha1.ProviderConfig, mcp client.Client) (ctrl.Result, error) {
	l := logf.FromContext(ctx)
	objs, err := renderKustomize()
	if err != nil {
		spruntime.StatusError(obj, "TemplatingFailed", err.Error())
		return ctrl.Result{}, err
	}
	// remove managed objs
	for _, domainObj := range objs {
		if err := mcp.Delete(ctx, domainObj); client.IgnoreNotFound(err) != nil {
			l.Error(err, "delete failed")
			spruntime.StatusError(obj, "DeleteFailed", "Object delete failed")
			return ctrl.Result{}, err
		}
		err := mcp.Get(ctx, client.ObjectKeyFromObject(domainObj), domainObj)
		if err == nil {
			// object still exists
			return ctrl.Result{
				RequeueAfter: time.Second * 10,
			}, nil
		}
		if apierrors.IsNotFound(err) {
			// object deleted
			continue
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FooServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	spReconciler := spruntime.Reconciler[*apiv1alpha1.FooService, *apiv1alpha1.ProviderConfig]{
		PlatformCluster:         r.PlatformCluster,
		ClusterAccessReconciler: r.ClusterAccessReconciler,
		Client:                  r.Client,
		Scheme:                  r.Scheme,
		DomainServiceReconciler: r,
		EmptyAPIObj: func() *apiv1alpha1.FooService {
			return &apiv1alpha1.FooService{}
		},
		EmptyProviderConfig: func() *apiv1alpha1.ProviderConfig {
			return &apiv1alpha1.ProviderConfig{}
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.FooService{}).
		Named("fooservice").
		Complete(&spReconciler)
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
