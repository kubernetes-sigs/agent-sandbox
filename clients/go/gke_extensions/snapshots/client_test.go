// Copyright 2026 The Kubernetes Authors.
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

package snapshots

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// ---------------------------------------------------------------------------
// fakeCRDDiscoverer
// ---------------------------------------------------------------------------

type fakeCRDDiscoverer struct {
	resources *metav1.APIResourceList
	err       error
}

func (f *fakeCRDDiscoverer) ServerResourcesForGroupVersion(_ string) (*metav1.APIResourceList, error) {
	return f.resources, f.err
}

func apiResourceList(kinds ...string) *metav1.APIResourceList {
	list := &metav1.APIResourceList{}
	for _, k := range kinds {
		list.APIResources = append(list.APIResources, metav1.APIResource{Kind: k})
	}
	return list
}

// ---------------------------------------------------------------------------
// checkSnapshotCRDInstalled
// ---------------------------------------------------------------------------

func TestCheckSnapshotCRDInstalled_Found(t *testing.T) {
	dc := &fakeCRDDiscoverer{resources: apiResourceList("PodSnapshot", "PodSnapshotManualTrigger")}
	if err := checkSnapshotCRDInstalled(dc, logr.Discard()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestCheckSnapshotCRDInstalled_NotFound(t *testing.T) {
	notFound := k8serrors.NewNotFound(schema.GroupResource{Group: PodSnapshotAPIGroup}, "")
	dc := &fakeCRDDiscoverer{err: notFound}
	err := checkSnapshotCRDInstalled(dc, logr.Discard())
	if !errors.Is(err, ErrCRDNotInstalled) {
		t.Errorf("expected ErrCRDNotInstalled, got %v", err)
	}
}

func TestCheckSnapshotCRDInstalled_Forbidden(t *testing.T) {
	forbidden := k8serrors.NewForbidden(schema.GroupResource{Group: PodSnapshotAPIGroup}, "", fmt.Errorf("access denied"))
	dc := &fakeCRDDiscoverer{err: forbidden}
	// 403 → assume installed, return nil
	if err := checkSnapshotCRDInstalled(dc, logr.Discard()); err != nil {
		t.Errorf("expected nil for forbidden, got %v", err)
	}
}

func TestCheckSnapshotCRDInstalled_KindMissing(t *testing.T) {
	dc := &fakeCRDDiscoverer{resources: apiResourceList("SomeOtherKind")}
	err := checkSnapshotCRDInstalled(dc, logr.Discard())
	if !errors.Is(err, ErrCRDNotInstalled) {
		t.Errorf("expected ErrCRDNotInstalled when PodSnapshot kind absent, got %v", err)
	}
}

func TestCheckSnapshotCRDInstalled_EmptyResourceList(t *testing.T) {
	dc := &fakeCRDDiscoverer{resources: &metav1.APIResourceList{}}
	err := checkSnapshotCRDInstalled(dc, logr.Discard())
	if !errors.Is(err, ErrCRDNotInstalled) {
		t.Errorf("expected ErrCRDNotInstalled for empty resource list, got %v", err)
	}
}

func TestCheckSnapshotCRDInstalled_OtherError(t *testing.T) {
	dc := &fakeCRDDiscoverer{err: fmt.Errorf("connection refused")}
	err := checkSnapshotCRDInstalled(dc, logr.Discard())
	if err == nil {
		t.Error("expected error for generic API failure")
	}
	if errors.Is(err, ErrCRDNotInstalled) {
		t.Error("generic connection error should not be wrapped as ErrCRDNotInstalled")
	}
}

func TestCheckSnapshotCRDInstalled_GroupDiscoveryFailed_NotFound(t *testing.T) {
	notFound := k8serrors.NewNotFound(schema.GroupResource{Group: PodSnapshotAPIGroup}, "")
	discoveryErr := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: PodSnapshotAPIGroup, Version: PodSnapshotAPIVersion}: notFound,
		},
	}
	dc := &fakeCRDDiscoverer{err: discoveryErr}
	err := checkSnapshotCRDInstalled(dc, logr.Discard())
	if !errors.Is(err, ErrCRDNotInstalled) {
		t.Errorf("expected ErrCRDNotInstalled for ErrGroupDiscoveryFailed wrapping 404, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// isGroupNotFound
// ---------------------------------------------------------------------------

func TestIsGroupNotFound_Nil(t *testing.T) {
	if isGroupNotFound(nil) {
		t.Error("expected false for nil error")
	}
}

func TestIsGroupNotFound_NonDiscoveryError(t *testing.T) {
	if isGroupNotFound(fmt.Errorf("unrelated error")) {
		t.Error("expected false for non-discovery error")
	}
}

func TestIsGroupNotFound_ErrGroupDiscoveryFailed_With404(t *testing.T) {
	notFound := k8serrors.NewNotFound(schema.GroupResource{}, "")
	derr := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "foo", Version: "v1"}: notFound,
		},
	}
	if !isGroupNotFound(derr) {
		t.Error("expected true when ErrGroupDiscoveryFailed wraps a 404")
	}
}

func TestIsGroupNotFound_ErrGroupDiscoveryFailed_NonNotFound(t *testing.T) {
	forbidden := k8serrors.NewForbidden(schema.GroupResource{}, "", fmt.Errorf("denied"))
	derr := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "foo", Version: "v1"}: forbidden,
		},
	}
	if isGroupNotFound(derr) {
		t.Error("expected false when ErrGroupDiscoveryFailed wraps a non-404 error")
	}
}

// ---------------------------------------------------------------------------
// Issue 6: PodSnapshotClient.opts field removed
// ---------------------------------------------------------------------------

func TestPodSnapshotClient_StructFields(t *testing.T) {
	// Guard against accidental re-introduction of the opts field using reflection
	// so a keyed literal that silently omits fields does not hide regressions.
	typ := reflect.TypeFor[PodSnapshotClient]()
	for f := range typ.Fields() {
		if f.Name == "opts" {
			t.Errorf("PodSnapshotClient must not have an 'opts' field (removed as dead state)")
		}
	}
	// Ensure the expected fields are still present.
	expected := map[string]bool{"inner": false, "k8s": false, "log": false}
	for f := range typ.Fields() {
		if _, ok := expected[f.Name]; ok {
			expected[f.Name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected field %q to exist on PodSnapshotClient", name)
		}
	}
	// Silence unused logr.Discard reference.
	_ = logr.Discard()
}
