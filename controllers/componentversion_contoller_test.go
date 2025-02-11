// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Open Component Model contributors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "github.com/open-component-model/ocm/pkg/contexts/datacontext/config"
	"github.com/open-component-model/ocm/pkg/contexts/ocm"
	ocmdesc "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/meta/v1"

	"github.com/open-component-model/ocm-controller/api/v1alpha1"
)

func TestComponentVersionReconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	err := v1alpha1.AddToScheme(scheme)
	assert.NoError(t, err)
	err = corev1.AddToScheme(scheme)
	assert.NoError(t, err)
	fakeClient := fake.NewClientBuilder()

	var (
		componentName = "test-name"
		secretName    = "test-secret"
		namespace     = "default"
	)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"creds": []byte("whatever"),
		},
	}
	obj := &v1alpha1.ComponentVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentName,
			Namespace: namespace,
		},
		Spec: v1alpha1.ComponentVersionSpec{
			Interval:  metav1.Duration{Duration: 10 * time.Minute},
			Component: "github.com/skarlso/root",
			Version:   "v0.0.1",
			Repository: v1alpha1.Repository{
				URL: "https://github.com/Skarlso/test",
				SecretRef: v1alpha1.SecretRef{
					Name: secretName,
				},
			},
			Verify: []v1alpha1.Signature{},
			References: v1alpha1.ReferencesConfig{
				Expand: true,
			},
		},
		Status: v1alpha1.ComponentVersionStatus{},
	}
	client := fakeClient.WithObjects(secret, obj).WithScheme(scheme).Build()
	root := &mockComponent{
		t: t,
		descriptor: &ocmdesc.ComponentDescriptor{
			ComponentSpec: ocmdesc.ComponentSpec{
				ObjectMeta: v1.ObjectMeta{
					Name:    "github.com/skarlso/root",
					Version: "v0.0.1",
				},
				References: ocmdesc.References{
					{
						ElementMeta: ocmdesc.ElementMeta{
							Name:    "test-ref-1",
							Version: "v0.0.1",
						},
						ComponentName: "github.com/skarlso/embedded",
					},
				},
			},
		},
	}
	embedded := &mockComponent{
		descriptor: &ocmdesc.ComponentDescriptor{
			ComponentSpec: ocmdesc.ComponentSpec{
				ObjectMeta: v1.ObjectMeta{
					Name:    "github.com/skarlso/embedded",
					Version: "v0.0.1",
				},
			},
		},
	}
	cvr := ComponentVersionReconciler{
		Scheme: scheme,
		Client: client,
		OCMClient: &mockFetcher{
			verified: true,
			t:        t,
			cv: map[string]ocm.ComponentVersionAccess{
				"github.com/skarlso/embedded": embedded,
				"github.com/skarlso/root":     root,
			},
		},
	}
	_, err = cvr.reconcile(context.Background(), obj)
	assert.NoError(t, err)
	assert.Len(t, obj.Status.ComponentDescriptor.References, 1)
	assert.Equal(t, "test-ref-1", obj.Status.ComponentDescriptor.References[0].Name)
}

type mockFetcher struct {
	getComponentErr error
	verifyErr       error
	cv              map[string]ocm.ComponentVersionAccess
	t               *testing.T
	verified        bool
}

func (m *mockFetcher) GetComponentVersion(ctx context.Context, obj *v1alpha1.ComponentVersion, name, version string) (ocm.ComponentVersionAccess, error) {
	m.t.Logf("called GetComponentVersion with name %s and version %s", name, version)
	return m.cv[name], m.getComponentErr
}

func (m *mockFetcher) VerifyComponent(ctx context.Context, obj *v1alpha1.ComponentVersion) (bool, error) {
	return m.verified, m.verifyErr
}

type mockComponent struct {
	descriptor *ocmdesc.ComponentDescriptor
	ocm.ComponentVersionAccess
	t *testing.T
}

func (m *mockComponent) GetDescriptor() *ocmdesc.ComponentDescriptor {
	return m.descriptor
}
