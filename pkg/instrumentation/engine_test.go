// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package instrumentation

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	colfeaturegate "go.opentelemetry.io/collector/featuregate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/aws/amazon-cloudwatch-agent-operator/apis/v1alpha1"
	"github.com/aws/amazon-cloudwatch-agent-operator/pkg/constants"
	"github.com/aws/amazon-cloudwatch-agent-operator/pkg/featuregate"
)

const testEngineEndpointArg = "--otlp-traces-endpoint=grpc://cloudwatch-agent.amazon-cloudwatch:4317"

func setEngineGate(t *testing.T, enabled bool) {
	t.Helper()
	was := featuregate.EnableEngineTracingSupport.IsEnabled()
	require.NoError(t, colfeaturegate.GlobalRegistry().Set(featuregate.EnableEngineTracingSupport.ID(), enabled))
	t.Cleanup(func() {
		require.NoError(t, colfeaturegate.GlobalRegistry().Set(featuregate.EnableEngineTracingSupport.ID(), was))
	})
}

func TestDetectEngine(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		declared  engineKind
		expected  engineKind
		found     bool
	}{
		{
			name:      "vllm image",
			container: corev1.Container{Name: "kserve-container", Image: "vllm/vllm-openai:v0.29.0"},
			expected:  engineVLLM,
			found:     true,
		},
		{
			name:      "vllm image on a private registry with a port",
			container: corev1.Container{Name: "engine", Image: "registry.example.com:5000/team/VLLM-Openai:latest"},
			expected:  engineVLLM,
			found:     true,
		},
		{
			name:      "vllm image by digest",
			container: corev1.Container{Name: "engine", Image: "vllm/vllm-openai@sha256:abc123"},
			expected:  engineVLLM,
			found:     true,
		},
		{
			name:      "sglang image",
			container: corev1.Container{Name: "engine", Image: "lmsysorg/sglang:v0.5.0-cu126"},
			expected:  engineSGLang,
			found:     true,
		},
		{
			name:      "another image from the vllm organization is not the engine",
			container: corev1.Container{Name: "router", Image: "vllm/router:0.1"},
			found:     false,
		},
		{
			name:      "an image naming both engines is ambiguous",
			container: corev1.Container{Name: "bench", Image: "acme/vllm-sglang-bench:1"},
			found:     false,
		},
		{
			name:      "no engine in the image and nothing declared",
			container: corev1.Container{Name: "kserve-container", Image: "my-registry/llm-runtime:1"},
			found:     false,
		},
		{
			name:      "declared engine on the kserve model container",
			container: corev1.Container{Name: "kserve-container", Image: "my-registry/llm-runtime:1"},
			declared:  engineSGLang,
			expected:  engineSGLang,
			found:     true,
		},
		{
			name:      "declared engine on another container is ignored",
			container: corev1.Container{Name: "sidecar", Image: "my-registry/llm-runtime:1"},
			declared:  engineSGLang,
			found:     false,
		},
		{
			name:      "unknown declared engine is ignored",
			container: corev1.Container{Name: "kserve-container", Image: "my-registry/llm-runtime:1"},
			declared:  engineKind("triton"),
			found:     false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, found := detectEngine(test.container, test.declared)
			assert.Equal(t, test.found, found)
			assert.Equal(t, test.expected, kind)
		})
	}
}

func TestEngineSkipReason(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		skipped   bool
	}{
		{
			name:      "args only",
			container: corev1.Container{Args: []string{"--model", "m"}},
		},
		{
			name:      "command and args",
			container: corev1.Container{Command: []string{"vllm", "serve"}, Args: []string{"m"}},
		},
		{
			name:      "command only",
			container: corev1.Container{Command: []string{"vllm", "serve", "m"}},
		},
		{
			name:      "shell command",
			container: corev1.Container{Command: []string{"/bin/sh", "-c", "vllm serve m"}},
			skipped:   true,
		},
		{
			name:      "no command and no args",
			container: corev1.Container{Image: "vllm/vllm-openai:v0.29.0"},
			skipped:   true,
		},
		{
			name:      "endpoint already set in args",
			container: corev1.Container{Args: []string{"--otlp-traces-endpoint=grpc://other:4317"}},
			skipped:   true,
		},
		{
			name:      "endpoint already set in command",
			container: corev1.Container{Command: []string{"vllm", "serve", "--otlp-traces-endpoint", "grpc://other:4317"}},
			skipped:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason := engineSkipReason(test.container)
			assert.Equal(t, test.skipped, reason != "", "reason was %q", reason)
		})
	}
}

func TestImageNameAndEndpoint(t *testing.T) {
	assert.Equal(t, "vllm-openai", imageName("vllm/vllm-openai:v0.29.0"))
	assert.Equal(t, "router", imageName("vllm/router:0.1"))
	assert.Equal(t, "sglang", imageName("registry.example.com:5000/lmsysorg/sglang:latest"))
	assert.Equal(t, "vllm-openai", imageName("vllm/vllm-openai@sha256:abc"))

	assert.Equal(t, "agent:4317", engineEndpointHostPort("agent:4317"))
	assert.Equal(t, "agent:4317", engineEndpointHostPort("grpc://agent:4317"))
	assert.Equal(t, "agent:4318", engineEndpointHostPort("http://agent:4318/"))
}

func TestHasFlag(t *testing.T) {
	assert.True(t, hasFlag([]string{"serve", "--enable-trace"}, "--enable-trace"))
	assert.True(t, hasFlag([]string{"--otlp-traces-endpoint=host:4317"}, "--otlp-traces-endpoint"))
	assert.False(t, hasFlag([]string{"--otlp-traces-endpoint-extra=1"}, "--otlp-traces-endpoint"))
	assert.False(t, hasFlag(nil, "--enable-trace"))
}

func TestInjectEngineTracesConfigVLLM(t *testing.T) {
	cfg := engineTracesConfig{endpoint: defaultEngineTracesEndpoint}
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name:  "kserve-container",
		Image: "vllm/vllm-openai:v0.29.0",
		Args:  []string{"--model", "Qwen/Qwen2.5-7B-Instruct"},
	}}}}

	pod = injectEngineTracesConfig(cfg, engineVLLM, pod, 0, pod.Spec.Containers[0].Env)

	assert.Equal(t, []string{"--model", "Qwen/Qwen2.5-7B-Instruct", testEngineEndpointArg}, pod.Spec.Containers[0].Args)
	// No service name and no resource attributes: the agent pipeline resolves both.
	assert.Equal(t, []corev1.EnvVar{
		{Name: envOtelExporterOTLPTracesProtocol, Value: "grpc"},
		{Name: constants.EnvOTELPropagators, Value: "tracecontext,baggage"},
	}, pod.Spec.Containers[0].Env)
}

func TestInjectEngineTracesConfigSGLang(t *testing.T) {
	cfg := engineTracesConfig{endpoint: "cloudwatch-agent.amazon-cloudwatch:4317"}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{kserveInferenceService: "ticket-triage"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "kserve-container",
			Image: "lmsysorg/sglang:latest",
			Args:  []string{"--model-path", "Qwen/Qwen2.5-7B-Instruct"},
		}}},
	}

	pod = injectEngineTracesConfig(cfg, engineSGLang, pod, 0, pod.Spec.Containers[0].Env)

	assert.Equal(t, []string{
		"--model-path", "Qwen/Qwen2.5-7B-Instruct",
		"--enable-trace",
		"--otlp-traces-endpoint=cloudwatch-agent.amazon-cloudwatch:4317",
	}, pod.Spec.Containers[0].Args)
	// SGLang names itself "sglang" when nothing else is set, so the workload name is sent.
	assert.Equal(t, "ticket-triage", getEnvValue(pod.Spec.Containers[0].Env, constants.EnvOTELServiceName))
}

func TestEngineServiceName(t *testing.T) {
	kserve := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{kserveInferenceService: "chat-assistant", "app.kubernetes.io/name": "other"},
	}}
	assert.Equal(t, "chat-assistant", engineServiceName(kserve, 0))

	appLabel := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"app.kubernetes.io/name": "search-answer"},
	}}
	assert.Equal(t, "search-answer", engineServiceName(appLabel, 0))

	owned := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "doc-summarizer-7d8kfgh9j2"}},
	}}
	assert.Equal(t, "doc-summarizer", engineServiceName(owned, 0))

	assert.Equal(t, "", engineServiceName(corev1.Pod{}, 0))
}

func TestInjectEngineTracesConfigSampler(t *testing.T) {
	cfg := engineTracesConfig{
		endpoint:   defaultEngineTracesEndpoint,
		sampler:    "parentbased_traceidratio",
		samplerArg: "0.1",
	}
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Args: []string{"--model", "m"}}}}}

	pod = injectEngineTracesConfig(cfg, engineVLLM, pod, 0, pod.Spec.Containers[0].Env)

	assert.Equal(t, "parentbased_traceidratio", getEnvValue(pod.Spec.Containers[0].Env, constants.EnvOTELTracesSampler))
	assert.Equal(t, "0.1", getEnvValue(pod.Spec.Containers[0].Env, constants.EnvOTELTracesSamplerArg))
}

func TestInjectEngineTracesConfigKeepsUserEnv(t *testing.T) {
	cfg := engineTracesConfig{endpoint: defaultEngineTracesEndpoint}
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "engine",
		Args: []string{"--model", "m"},
		Env:  []corev1.EnvVar{{Name: constants.EnvOTELPropagators, Value: "xray"}},
	}}}}

	pod = injectEngineTracesConfig(cfg, engineVLLM, pod, 0, pod.Spec.Containers[0].Env)

	assert.Equal(t, "xray", getEnvValue(pod.Spec.Containers[0].Env, constants.EnvOTELPropagators))
	assert.Equal(t, 2, len(pod.Spec.Containers[0].Env))
}

func enginePod(annotations map[string]string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "chat-assistant-predictor-abc", Namespace: "chat", Annotations: annotations},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "kserve-container",
			Image: "vllm/vllm-openai:v0.29.0",
			Args:  []string{"--model", "Qwen/Qwen2.5-7B-Instruct"},
		}}},
	}
}

func engineMutator() *instPodMutator {
	return &instPodMutator{
		Logger:      logr.Discard(),
		sdkInjector: &sdkInjector{logger: logr.Discard()},
	}
}

func TestInjectEngineTraces(t *testing.T) {
	on := map[string]string{annotationInjectEngineTraces: "true"}

	t.Run("feature gate off", func(t *testing.T) {
		setEngineGate(t, false)
		pod := enginePod(on)
		expected := pod.DeepCopy()

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, pod)

		assert.Equal(t, expected.Spec.Containers[0], pod.Spec.Containers[0])
	})

	t.Run("no annotation, so the gate decides", func(t *testing.T) {
		setEngineGate(t, true)

		pod := engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, enginePod(nil))

		assert.Contains(t, pod.Spec.Containers[0].Args, testEngineEndpointArg)
	})

	t.Run("annotation false", func(t *testing.T) {
		setEngineGate(t, true)
		pod := enginePod(map[string]string{annotationInjectEngineTraces: "false"})
		expected := pod.DeepCopy()

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, pod)

		assert.Equal(t, expected.Spec.Containers[0], pod.Spec.Containers[0])
	})

	t.Run("namespace opts out", func(t *testing.T) {
		setEngineGate(t, true)
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        "chat",
			Annotations: map[string]string{annotationInjectEngineTraces: "false"},
		}}
		pod := enginePod(nil)
		expected := pod.DeepCopy()

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), ns, pod)

		assert.Equal(t, expected.Spec.Containers[0], pod.Spec.Containers[0])
	})

	t.Run("the operator setting names the endpoint", func(t *testing.T) {
		setEngineGate(t, true)
		t.Setenv(EnvEngineTracesEndpoint, "cloudwatch-agent.observability:14317")

		pod := engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, enginePod(nil))

		assert.Contains(t, pod.Spec.Containers[0].Args, "--otlp-traces-endpoint=grpc://cloudwatch-agent.observability:14317")
	})

	t.Run("endpoint override with a scheme is normalized and wins", func(t *testing.T) {
		setEngineGate(t, true)
		t.Setenv(EnvEngineTracesEndpoint, "cloudwatch-agent.observability:14317")
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        "chat",
			Annotations: map[string]string{annotationEngineTracesEndpoint: "http://10.0.0.1:4317"},
		}}

		pod := engineMutator().injectEngineTraces(context.Background(), logr.Discard(), ns, enginePod(nil))

		assert.Contains(t, pod.Spec.Containers[0].Args, "--otlp-traces-endpoint=grpc://10.0.0.1:4317")
	})

	t.Run("only the engine container in a pod of several", func(t *testing.T) {
		setEngineGate(t, true)
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "predictor-abc", Namespace: "chat"},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "storage-initializer", Image: "kserve/storage-initializer:v0.14"}},
				Containers: []corev1.Container{
					{Name: "queue-proxy", Image: "knative/queue:1.19", Args: []string{"--serve"}},
					{Name: "kserve-container", Image: "vllm/vllm-openai:v0.29.0", Args: []string{"--model", "m"}},
					{Name: "router", Image: "vllm/router:0.1", Args: []string{"--listen"}},
				},
			},
		}
		expected := pod.DeepCopy()

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, pod)

		assert.Equal(t, expected.Spec.Containers[0], pod.Spec.Containers[0], "queue-proxy")
		assert.Equal(t, expected.Spec.Containers[2], pod.Spec.Containers[2], "router")
		assert.Equal(t, expected.Spec.InitContainers[0], pod.Spec.InitContainers[0], "init container")
		assert.Contains(t, pod.Spec.Containers[1].Args, testEngineEndpointArg)
	})

	t.Run("a second pass changes nothing", func(t *testing.T) {
		setEngineGate(t, true)

		once := engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, enginePod(nil))
		twice := engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, *once.DeepCopy())

		assert.Equal(t, once, twice)
	})

	t.Run("language auto-instrumentation stays possible", func(t *testing.T) {
		setEngineGate(t, true)
		pod := enginePod(nil)
		require.False(t, isAutoInstrumentationInjected(pod))

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, pod)

		// The guard in Mutate keys on the node-name env var that the language path adds. If
		// the engine path added it, every engine pod would lose its language SDK.
		assert.False(t, isAutoInstrumentationInjected(pod))
		assert.Equal(t, "", getEnvValue(pod.Spec.Containers[0].Env, constants.EnvNodeName))
	})

	t.Run("container that is not an engine", func(t *testing.T) {
		setEngineGate(t, true)
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "chat"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27", Args: []string{"-g", "daemon off;"}}}},
		}
		expected := pod.DeepCopy()

		pod = engineMutator().injectEngineTraces(context.Background(), logr.Discard(), corev1.Namespace{}, pod)

		assert.Equal(t, expected.Spec.Containers[0], pod.Spec.Containers[0])
	})
}

// TestMutateEngineAndPython covers the whole webhook path. An engine pod that also asks for
// Python auto-instrumentation gets the engine flag, and the Python SDK is turned off for it:
// the distro would own the global tracer provider and the engine's spans would never leave.
// With the feature gate off, the same pod keeps its Python instrumentation.
func TestMutateEngineAndPython(t *testing.T) {
	mutator := NewMutator(logr.Discard(), k8sClient, record.NewFakeRecorder(100))
	require.NotNil(t, mutator)

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "engine-tracing-test"}}
	require.NoError(t, k8sClient.Create(context.Background(), &ns))
	defer func() {
		_ = k8sClient.Delete(context.Background(), &ns)
	}()
	inst := v1alpha1.Instrumentation{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-tracing-inst", Namespace: ns.Name},
		Spec: v1alpha1.InstrumentationSpec{
			Python:   v1alpha1.Python{Image: "python-instrumentation:1"},
			Exporter: v1alpha1.Exporter{Endpoint: "http://collector:4317"},
		},
	}
	require.NoError(t, k8sClient.Create(context.Background(), &inst))

	initContainerNames := func(pod corev1.Pod) []string {
		var names []string
		for _, container := range pod.Spec.InitContainers {
			names = append(names, container.Name)
		}
		return names
	}

	t.Run("engine tracing on", func(t *testing.T) {
		setEngineGate(t, true)
		pod := enginePod(map[string]string{annotationInjectPython: "true"})
		pod.Namespace = ns.Name

		mutated, err := mutator.Mutate(context.Background(), ns, pod)
		require.NoError(t, err)

		assert.Contains(t, mutated.Spec.Containers[0].Args, testEngineEndpointArg)
		assert.Equal(t, "false", mutated.Annotations[annotationInjectPython])
		assert.NotContains(t, initContainerNames(mutated), pythonInitContainerName)
	})

	t.Run("engine tracing on, Application Signals kept", func(t *testing.T) {
		setEngineGate(t, true)
		t.Setenv(EnvEngineTracesKeepAppSignals, "true")
		pod := enginePod(map[string]string{annotationInjectPython: "true"})
		pod.Namespace = ns.Name

		mutated, err := mutator.Mutate(context.Background(), ns, pod)
		require.NoError(t, err)

		assert.Contains(t, mutated.Spec.Containers[0].Args, testEngineEndpointArg)
		assert.Contains(t, initContainerNames(mutated), pythonInitContainerName)
	})

	t.Run("engine tracing off", func(t *testing.T) {
		setEngineGate(t, false)
		pod := enginePod(map[string]string{annotationInjectPython: "true"})
		pod.Namespace = ns.Name

		mutated, err := mutator.Mutate(context.Background(), ns, pod)
		require.NoError(t, err)

		assert.NotContains(t, mutated.Spec.Containers[0].Args, testEngineEndpointArg)
		assert.Contains(t, initContainerNames(mutated), pythonInitContainerName)
	})
}
