// Copyright 2022.
// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Open Component Model contributors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"strings"

	"github.com/fluxcd/pkg/apis/meta"
	hash "github.com/mitchellh/hashstructure"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ocmclient "github.com/open-component-model/ocm-controller/pkg/ocm"
	ocmdesc "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc"
	v1 "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/meta/v1"
	compdesc "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/versions/ocm.software/v3alpha1"

	"github.com/open-component-model/ocm-controller/api/v1alpha1"
)

// ComponentVersionReconciler reconciles a ComponentVersion object
type ComponentVersionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	OCMClient ocmclient.FetchVerifier
}

//+kubebuilder:rbac:groups=delivery.ocm.software,resources=componentversions;componentdescriptors,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=delivery.ocm.software,resources=componentversions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=delivery.ocm.software,resources=componentversions/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=secrets;serviceaccounts,verbs=get;list;watch

// SetupWithManager sets up the controller with the Manager.
func (r *ComponentVersionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ComponentVersion{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ComponentVersionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("ocm-component-version-reconcile")

	log.Info("starting ocm component loop")

	component := &v1alpha1.ComponentVersion{}
	if err := r.Client.Get(ctx, req.NamespacedName, component); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get component object: %w", err)
	}

	log.V(4).Info("found component", "component", component)

	log.Info("running verification of component")
	ok, err := r.OCMClient.VerifyComponent(ctx, component)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: component.GetRequeueAfter(),
		}, fmt.Errorf("failed to verify component: %w", err)
	}

	if !ok {
		return ctrl.Result{
			RequeueAfter: component.GetRequeueAfter(),
		}, fmt.Errorf("attempted to verify component, but the digest didn't match")
	}

	return r.reconcile(ctx, component)
}

func (r *ComponentVersionReconciler) reconcile(ctx context.Context, obj *v1alpha1.ComponentVersion) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("ocm-component-version-reconcile")

	// get component version
	cv, err := r.OCMClient.GetComponentVersion(ctx, obj, obj.Spec.Component, obj.Spec.Version)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to get component version: %w", err)
	}

	// convert ComponentDescriptor to v3alpha1
	dv := &compdesc.DescriptorVersion{}
	cd, err := dv.ConvertFrom(cv.GetDescriptor())
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to convret component descriptor: %w", err)
	}

	// setup the component descriptor kubernetes resource
	componentName, err := r.constructComponentName(cd.GetName(), cd.GetVersion(), nil)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to generate name: %w", err)
	}
	descriptor := &v1alpha1.ComponentDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: obj.GetNamespace(),
			Name:      componentName,
		},
	}

	// create or update the component descriptor kubernetes resource
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, descriptor, func() error {
		if descriptor.ObjectMeta.CreationTimestamp.IsZero() {
			if err := controllerutil.SetOwnerReference(obj, descriptor, r.Scheme); err != nil {
				return fmt.Errorf("failed to set owner reference: %w", err)
			}
		}
		spec := v1alpha1.ComponentDescriptorSpec{
			ComponentVersionSpec: cd.(*compdesc.ComponentDescriptor).Spec,
			Version:              cd.GetVersion(),
		}
		descriptor.Spec = spec
		return nil
	})

	if err != nil {
		return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()},
			fmt.Errorf("failed to create or update component descriptor: %w", err)
	}

	componentDescriptor := v1alpha1.Reference{
		Name:    cd.GetName(),
		Version: cd.GetVersion(),
		ComponentDescriptorRef: meta.NamespacedObjectReference{
			Name:      descriptor.GetName(),
			Namespace: descriptor.GetNamespace(),
		},
	}

	log.V(4).Info("successfully completed mutation", "operation", op)

	// if references.expand is false then return here
	if !obj.Spec.References.Expand {
		return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, err
	}

	componentDescriptor.References, err = r.parseReferences(ctx, obj, cv.GetDescriptor().References)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to get references: %w", err)
	}

	// initialize the patch helper
	patchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to create patch helper: %w", err)
	}

	obj.Status.ComponentDescriptor = componentDescriptor

	if err := patchHelper.Patch(ctx, obj); err != nil {
		return ctrl.Result{
			RequeueAfter: obj.GetRequeueAfter(),
		}, fmt.Errorf("failed to patch resource: %w", err)
	}

	log.Info("reconciliation complete")
	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

// parseReferences takes a list of references to embedded components and constructs a dependency tree out of them.
func (r *ComponentVersionReconciler) parseReferences(ctx context.Context, parent *v1alpha1.ComponentVersion, references ocmdesc.References) ([]v1alpha1.Reference, error) {
	log := log.FromContext(ctx)
	result := make([]v1alpha1.Reference, 0)
	for _, ref := range references {
		// get component version
		rcv, err := r.OCMClient.GetComponentVersion(ctx, parent, ref.ComponentName, ref.Version)
		if err != nil {
			return nil, fmt.Errorf("failed to get component version: %w", err)
		}
		// convert ComponentDescriptor to v3alpha1
		dv := &compdesc.DescriptorVersion{}
		cd, err := dv.ConvertFrom(rcv.GetDescriptor())
		if err != nil {
			return nil, fmt.Errorf("failed to convret component descriptor: %w", err)
		}
		// setup the component descriptor kubernetes resource
		componentName, err := r.constructComponentName(ref.ComponentName, ref.Version, ref.GetMeta().ExtraIdentity)
		if err != nil {
			return nil, fmt.Errorf("failed to generate name: %w", err)
		}
		descriptor := &v1alpha1.ComponentDescriptor{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: parent.GetNamespace(),
				Name:      componentName,
			},
			Spec: v1alpha1.ComponentDescriptorSpec{
				ComponentVersionSpec: cd.(*compdesc.ComponentDescriptor).Spec,
				Version:              ref.Version,
			},
		}

		if err := controllerutil.SetOwnerReference(parent, descriptor, r.Scheme); err != nil {
			return nil, fmt.Errorf("failed to set owner reference: %w", err)
		}

		// create or update the component descriptor kubernetes resource
		// we don't need to update it
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, descriptor, func() error {
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create/update component descriptor: %w", err)
		}
		log.V(4).Info(fmt.Sprintf("%s(ed) descriptor", op), "descriptor", klog.KObj(descriptor))

		reference := v1alpha1.Reference{
			Name:    ref.Name,
			Version: ref.Version,
			ComponentDescriptorRef: meta.NamespacedObjectReference{
				Name:      descriptor.Name,
				Namespace: descriptor.Namespace,
			},
			ExtraIdentity: ref.ExtraIdentity,
		}

		if len(rcv.GetDescriptor().References) > 0 {
			out, err := r.parseReferences(ctx, parent, rcv.GetDescriptor().References)
			if err != nil {
				return nil, err
			}
			reference.References = out
		}
		result = append(result, reference)
	}
	return result, nil
}

// constructComponentName constructs a unique name from a component name and version.
func (r *ComponentVersionReconciler) constructComponentName(name, version string, identity v1.Identity) (string, error) {
	namingScheme := struct {
		componentName string
		version       string
		identity      v1.Identity
	}{
		componentName: name,
		version:       version,
		identity:      identity,
	}
	h, err := hash.Hash(namingScheme, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate hash for name, version, identity: %w", err)
	}
	return fmt.Sprintf("%s-%s-%d", strings.ReplaceAll(name, "/", "-"), version, h), nil
}
