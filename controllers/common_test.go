// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws/amazon-cloudwatch-agent-operator/apis/v1alpha1"
)

// otelConfigWithReceivers builds a minimal otelConfig whose metrics pipeline reads receivers.
func otelConfigWithReceivers(receivers ...string) string {
	conf := "receivers:\n"
	for _, r := range receivers {
		conf += "  " + r + ":\n"
	}
	conf += "service:\n  pipelines:\n    metrics/cw_k8s_ci_v0_test:\n      receivers:\n"
	for _, r := range receivers {
		conf += "        - " + r + "\n"
	}
	return conf
}

func TestEnabledAcceleratedComputeByAgentConfig(t *testing.T) {
	logger := logf.Log.WithName("unit-tests")
	testCases := []struct {
		name     string
		config   string
		expected bool
	}{
		{
			name:     "disabledEnhancedContainerInsights",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":false}}}}`,
			expected: false,
		},
		{
			name:     "missingAcceleratedComputeMetric",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":true}}}}`,
			expected: true,
		},
		{
			name:     "disabledAcceleratedComputeMetric",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":true, "accelerated_compute_metrics":false}}}}`,
			expected: false,
		},
		{
			name:     "enabledAcceleratedComputeMetric",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":true, "accelerated_compute_metrics":true}}}}`,
			expected: true,
		},
		{
			name:     "mixedCaseWithDisabledEnhanced",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":false, "accelerated_compute_metrics":true}}}}`,
			expected: false,
		},
		{
			name:     "missingKubernetesBlock",
			config:   `{"logs":{"metrics_collected":{}}}`,
			expected: false,
		},
		{
			name:     "malformed",
			config:   `"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":false, "accelerated_compute_metrics":true}}}}`,
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := enabledAcceleratedComputeByAgentConfig(tc.config, logger)
			assert.Equal(t, tc.expected, actual)
		})
	}
}

func TestEnabledAcceleratedComputeByOtelConfig(t *testing.T) {
	logger := logf.Log.WithName("unit-tests")
	testCases := []struct {
		name       string
		otelConfig string
		receiver   string
		expected   bool
	}{
		{
			name:       "empty",
			otelConfig: "",
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
		{
			name:       "emptyMapping",
			otelConfig: "{}",
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
		{
			name:       "receiverInPipeline",
			otelConfig: otelConfigWithReceivers(dcgmExporterOtelReceiver),
			receiver:   dcgmExporterOtelReceiver,
			expected:   true,
		},
		{
			name:       "otherAcceleratorOnly",
			otelConfig: otelConfigWithReceivers(neuronMonitorOtelReceiver),
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
		{
			name:       "bothAccelerators",
			otelConfig: otelConfigWithReceivers(dcgmExporterOtelReceiver, neuronMonitorOtelReceiver),
			receiver:   neuronMonitorOtelReceiver,
			expected:   true,
		},
		{
			// A receiver no pipeline references is never started, so the exporter has no reader.
			name:       "receiverDeclaredButNotPiped",
			otelConfig: "receivers:\n  " + dcgmExporterOtelReceiver + ":\nservice:\n  pipelines:\n    metrics/other:\n      receivers:\n        - prometheus/cw_k8s_ci_v0_cadvisor\n",
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
		{
			name:       "noServiceSection",
			otelConfig: "receivers:\n  " + dcgmExporterOtelReceiver + ":\n",
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
		{
			name:       "malformed",
			otelConfig: "service:\n  pipelines:\n   - this is not a mapping\n  invalid",
			receiver:   dcgmExporterOtelReceiver,
			expected:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := enabledAcceleratedComputeByOtelConfig(tc.otelConfig, tc.receiver, logger)
			assert.Equal(t, tc.expected, actual)
		})
	}
}

func TestEnabledAcceleratedCompute(t *testing.T) {
	ctx := context.Background()
	logger := logf.Log.WithName("unit-tests")
	enhancedCI := `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":true}}}}`
	appSignalsOnly := `{"agent":{"region":"us-west-2"},"logs":{"metrics_collected":{"application_signals":{}}}}`

	testCases := []struct {
		name       string
		config     string
		otelConfig string
		receiver   string
		expected   bool
	}{
		{
			name:     "neitherPath",
			config:   appSignalsOnly,
			receiver: dcgmExporterOtelReceiver,
			expected: false,
		},
		{
			name:     "enhancedContainerInsightsOnly",
			config:   enhancedCI,
			receiver: dcgmExporterOtelReceiver,
			expected: true,
		},
		{
			name:       "otelContainerInsightsOnly",
			config:     appSignalsOnly,
			otelConfig: otelConfigWithReceivers(dcgmExporterOtelReceiver),
			receiver:   dcgmExporterOtelReceiver,
			expected:   true,
		},
		{
			name:       "otelContainerInsightsWithoutThisAccelerator",
			config:     appSignalsOnly,
			otelConfig: otelConfigWithReceivers(dcgmExporterOtelReceiver),
			receiver:   neuronMonitorOtelReceiver,
			expected:   false,
		},
		{
			name:       "bothPaths",
			config:     enhancedCI,
			otelConfig: otelConfigWithReceivers(dcgmExporterOtelReceiver),
			receiver:   dcgmExporterOtelReceiver,
			expected:   true,
		},
		{
			// Legacy opt-out still wins when OTEL is not collecting this accelerator.
			name:     "acceleratedComputeMetricsExplicitlyDisabled",
			config:   `{"logs":{"metrics_collected":{"kubernetes":{"enhanced_container_insights":true,"accelerated_compute_metrics":false}}}}`,
			receiver: dcgmExporterOtelReceiver,
			expected: false,
		},
	}

	original := getAmazonCloudWatchAgentResource
	t.Cleanup(func() { getAmazonCloudWatchAgentResource = original })

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			getAmazonCloudWatchAgentResource = func(ctx context.Context, c client.Client) v1alpha1.AmazonCloudWatchAgent {
				return v1alpha1.AmazonCloudWatchAgent{
					ObjectMeta: metav1.ObjectMeta{},
					Spec: v1alpha1.AmazonCloudWatchAgentSpec{
						Config:     tc.config,
						OtelConfig: tc.otelConfig,
					},
				}
			}
			actual := enabledAcceleratedCompute(ctx, nil, logger, tc.receiver)
			assert.Equal(t, tc.expected, actual)
		})
	}
}
