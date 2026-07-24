// Copyright 2025 The Kubernetes Authors.
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

package rawpatch

import (
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAnnotationsByteExact pins the exact wire payload: sorted keys, no
// whitespace, JSON string escaping.
func TestAnnotationsByteExact(t *testing.T) {
	tests := []struct {
		name string
		kv   map[string]string
		want string
	}{
		{
			name: "single key",
			kv:   map[string]string{"agents.x-k8s.io/pod-name": "my-pod-abc12"},
			want: `{"metadata":{"annotations":{"agents.x-k8s.io/pod-name":"my-pod-abc12"}}}`,
		},
		{
			name: "two keys emitted in sorted order regardless of insertion order",
			kv: map[string]string{
				"z.example.com/observed-at": "2026-07-19T15:27:56.85Z",
				"a.example.com/trace":       "00-abc-def-01",
			},
			want: `{"metadata":{"annotations":{"a.example.com/trace":"00-abc-def-01","z.example.com/observed-at":"2026-07-19T15:27:56.85Z"}}}`,
		},
		{
			name: "value escaping (encoding/json HTML-escapes, same as MergeFrom)",
			kv:   map[string]string{"k": "line1\nline2 \"quoted\" <tag>"},
			want: "{\"metadata\":{\"annotations\":{\"k\":\"line1\\nline2 \\\"quoted\\\" \\u003ctag\\u003e\"}}}",
		},
		{
			name: "empty value is still a set, not a delete",
			kv:   map[string]string{"k": ""},
			want: `{"metadata":{"annotations":{"k":""}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Annotations(tt.kv)
			if err != nil {
				t.Fatalf("Annotations() error: %v", err)
			}
			if p.Type() != types.MergePatchType {
				t.Fatalf("patch type = %v, want %v", p.Type(), types.MergePatchType)
			}
			data, err := p.Data(nil)
			if err != nil {
				t.Fatalf("Data() error: %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("payload mismatch:\n got: %s\nwant: %s", data, tt.want)
			}
		})
	}
}

func TestLabelsByteExact(t *testing.T) {
	p, err := Labels(map[string]string{"agents.x-k8s.io/launch-type": "warm"})
	if err != nil {
		t.Fatalf("Labels() error: %v", err)
	}
	data, err := p.Data(nil)
	if err != nil {
		t.Fatalf("Data() error: %v", err)
	}
	want := `{"metadata":{"labels":{"agents.x-k8s.io/launch-type":"warm"}}}`
	if string(data) != want {
		t.Errorf("payload mismatch:\n got: %s\nwant: %s", data, want)
	}
}

func TestEmptySetRejected(t *testing.T) {
	if _, err := Annotations(nil); err == nil {
		t.Error("Annotations(nil) should error")
	}
	if _, err := Labels(map[string]string{}); err == nil {
		t.Error("Labels(empty) should error")
	}
}

// TestEquivalenceWithMergeFrom proves the raw payload is byte-identical to
// what the DeepCopy+client.MergeFrom pattern produced for the same
// metadata-only mutation — i.e. the optimization changes nothing on the wire.
func TestEquivalenceWithMergeFrom(t *testing.T) {
	base := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{"existing": "label"},
			Annotations: map[string]string{
				"existing/anno": "value",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "c", Image: "debian:latest"}},
		},
	}

	t.Run("annotations", func(t *testing.T) {
		add := map[string]string{
			"agents.x-k8s.io/pod-name": "pod-1",
			"trace/context":            "00-ff-01",
			"escape/check":             "a<b>&\"c\"\n",
		}
		modified := base.DeepCopy()
		legacy := client.MergeFrom(base.DeepCopy())
		maps.Copy(modified.Annotations, add)
		legacyData, err := legacy.Data(modified)
		if err != nil {
			t.Fatalf("MergeFrom Data() error: %v", err)
		}

		raw, err := Annotations(add)
		if err != nil {
			t.Fatalf("Annotations() error: %v", err)
		}
		rawData, err := raw.Data(modified)
		if err != nil {
			t.Fatalf("raw Data() error: %v", err)
		}
		if string(rawData) != string(legacyData) {
			t.Errorf("wire payloads differ:\n raw:   %s\n merge: %s", rawData, legacyData)
		}
	})

	t.Run("labels", func(t *testing.T) {
		add := map[string]string{"agents.x-k8s.io/launch-type": "warm"}
		modified := base.DeepCopy()
		legacy := client.MergeFrom(base.DeepCopy())
		maps.Copy(modified.Labels, add)
		legacyData, err := legacy.Data(modified)
		if err != nil {
			t.Fatalf("MergeFrom Data() error: %v", err)
		}

		raw, err := Labels(add)
		if err != nil {
			t.Fatalf("Labels() error: %v", err)
		}
		rawData, err := raw.Data(modified)
		if err != nil {
			t.Fatalf("raw Data() error: %v", err)
		}
		if string(rawData) != string(legacyData) {
			t.Errorf("wire payloads differ:\n raw:   %s\n merge: %s", rawData, legacyData)
		}
	})
}
