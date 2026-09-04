package httpapi

import (
	"testing"
)

func TestAgentMetricsUseOnlyBoundedLabels(t *testing.T) {
	metrics := NewMetrics()
	metrics.observeAgentHeartbeat("accepted")
	metrics.setAgentHealth(1, 2, 3)
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "restfleet_agent_heartbeats_total" &&
			family.GetName() != "restfleet_agents" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "result" && label.GetName() != "health" {
					t.Fatalf("%s has unbounded label %q", family.GetName(), label.GetName())
				}
			}
		}
	}
}
