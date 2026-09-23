package kube

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestAccessConditionsKeepUnknownSeparateFromNotReady(t *testing.T) {
	cases := []struct {
		name       string
		conditions []any
		want       string
	}{
		{name: "all true", conditions: []any{map[string]any{"type": "Accepted", "status": "True"}, map[string]any{"type": "Programmed", "status": "True"}}, want: "ready"},
		{name: "controller pending", conditions: []any{map[string]any{"type": "Accepted", "status": "True"}}, want: "unknown"},
		{name: "controller rejected", conditions: []any{map[string]any{"type": "Accepted", "status": "False"}, map[string]any{"type": "Programmed", "status": "Unknown"}}, want: "not_ready"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": item.conditions}}}
			if got := readyConditions(object, "Accepted", "Programmed"); got != item.want {
				t.Fatalf("got %q, want %q", got, item.want)
			}
		})
	}
	stale := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"generation": int64(3)},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}},
	}}
	if got := readyConditions(stale, "Ready"); got != "unknown" {
		t.Fatalf("stale controller observation must be unknown, got %q", got)
	}
}
