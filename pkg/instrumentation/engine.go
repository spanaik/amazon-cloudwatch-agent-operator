// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package instrumentation

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/aws/amazon-cloudwatch-agent-operator/pkg/constants"
	"github.com/aws/amazon-cloudwatch-agent-operator/pkg/featuregate"
)

// The operator's engine-traces settings reach the pod mutator through the environment, the
// way the auto-instrumentation settings travel. A pod or namespace annotation overrides each.
const (
	EnvEngineTracesEndpoint   = "ENGINE_TRACES_ENDPOINT"
	EnvEngineTracesSampler    = "ENGINE_TRACES_SAMPLER"
	EnvEngineTracesSamplerArg = "ENGINE_TRACES_SAMPLER_ARG"

	// EnvEngineTracesKeepAppSignals leaves Python auto-instrumentation on the engine
	// container. Set it only for an engine that exports from the provider it configured
	// rather than from the global one, which no released vLLM or SGLang does yet.
	EnvEngineTracesKeepAppSignals = "ENGINE_TRACES_KEEP_APPLICATION_SIGNALS"
)

const (
	annotationInjectEngineTraces     = "cloudwatch.aws.amazon.com/inject-engine-traces"
	annotationEngineTracesEndpoint   = "cloudwatch.aws.amazon.com/engine-traces-endpoint"
	annotationEngineTracesEngine     = "cloudwatch.aws.amazon.com/engine-traces-engine"
	annotationEngineTracesSampler    = "cloudwatch.aws.amazon.com/engine-traces-sampler"
	annotationEngineTracesSamplerArg = "cloudwatch.aws.amazon.com/engine-traces-sampler-arg"

	// The node agent serves OTLP/gRPC here once the otlp receiver is enabled.
	defaultEngineTracesEndpoint = "cloudwatch-agent.amazon-cloudwatch:4317"

	// KServe names the model container this in both RawDeployment and Serverless mode.
	kserveModelContainerName = "kserve-container"
	kserveInferenceService   = "serving.kserve.io/inferenceservice"

	engineTracesEndpointFlag = "--otlp-traces-endpoint"
	sglangEnableTraceFlag    = "--enable-trace"

	engineTracesProtocol    = "grpc"
	engineTracesPropagators = "tracecontext,baggage"
)

// engineKind is a model server whose own OpenTelemetry tracing this package can turn on.
type engineKind string

const (
	engineVLLM   engineKind = "vllm"
	engineSGLang engineKind = "sglang"
)

// shells whose command line this package must not extend. Appending a flag to
// "sh -c <script>" passes it as the shell's $0, where nothing reads it.
var engineShells = []string{"sh", "bash", "dash", "ash", "zsh"}

type engineTracesConfig struct {
	endpoint   string
	engine     engineKind
	sampler    string
	samplerArg string
}

// injectEngineTraces turns on the model server's own tracing and points it at the node
// agent. Three things make this different from the language injectors. The engines read a
// command-line argument rather than an environment variable, so the flag goes on Args. Their
// images already carry the OTLP exporter, so nothing is mounted. And the agent's own pipeline
// resolves service identity for what it receives, so this writes no service.name for vLLM
// and no resource attributes at all.
func (pm *instPodMutator) injectEngineTraces(ctx context.Context, logger logr.Logger, ns corev1.Namespace, pod corev1.Pod) corev1.Pod {
	if !featuregate.EnableEngineTracingSupport.IsEnabled() {
		return pod
	}
	// The feature gate is the switch, so that a cluster-wide setting reaches every engine with
	// no manifest change. The annotation is the way out, on the pod or on its namespace.
	if strings.EqualFold(annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationInjectEngineTraces), "false") {
		return pod
	}

	cfg := engineTracesConfig{
		endpoint:   defaultEngineTracesEndpoint,
		engine:     engineKind(strings.ToLower(annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationEngineTracesEngine))),
		sampler:    os.Getenv(EnvEngineTracesSampler),
		samplerArg: os.Getenv(EnvEngineTracesSamplerArg),
	}
	if endpoint := os.Getenv(EnvEngineTracesEndpoint); endpoint != "" {
		cfg.endpoint = endpoint
	}
	if endpoint := annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationEngineTracesEndpoint); endpoint != "" {
		cfg.endpoint = endpoint
	}
	if sampler := annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationEngineTracesSampler); sampler != "" {
		cfg.sampler = sampler
		cfg.samplerArg = annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationEngineTracesSamplerArg)
	}
	cfg.endpoint = engineEndpointHostPort(cfg.endpoint)

	configMapCache := map[string]*corev1.ConfigMap{}
	for index := range pod.Spec.Containers {
		container := pod.Spec.Containers[index]
		kind, found := detectEngine(container, cfg.engine)
		if !found {
			continue
		}
		if reason := engineSkipReason(container); reason != "" {
			logger.V(1).Info("skipping engine tracing", "container", container.Name, "reason", reason)
			continue
		}
		// A value set from a ConfigMap or Secret counts as set by the user, and an env entry
		// would override it, so resolve envFrom before the guards read the list.
		envs := container.Env
		if len(container.EnvFrom) > 0 {
			envs = getAllEnvVars(ctx, pm.Client, &pod.Spec.Containers[index], ns.Name, logger, configMapCache)
		}
		pod = injectEngineTracesConfig(cfg, kind, pod, index, envs)
		if !strings.EqualFold(os.Getenv(EnvEngineTracesKeepAppSignals), "true") {
			pod = suppressPythonInstrumentation(pod)
		}
		logger.V(1).Info("injected engine tracing",
			"container", container.Name, "engine", string(kind), "endpoint", cfg.endpoint)
	}
	return pod
}

// suppressPythonInstrumentation turns the Python SDK injection off for this pod, measured as
// the condition for engine tracing to work at all. Both engines create their spans from the
// global tracer provider, and the ADOT distro installs that provider first, so the engine's
// own exporter then receives nothing: measured on 2026-11-23, zero engine spans with the
// distro present and spans flowing as soon as it was gone. The annotation is written before
// the language path reads it, in the same admission pass, and only the Python SDK is turned
// off: the other languages install no Python tracer provider.
func suppressPythonInstrumentation(pod corev1.Pod) corev1.Pod {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annotationInjectPython] = "false"
	return pod
}

// detectEngine names the model server running in a container. The image name is the only
// automatic signal, because the two engines take different arguments and the container name
// alone does not say which one is inside. A private image that carries no hint is matched by
// the engine annotation instead, on the KServe model container.
func detectEngine(container corev1.Container, declared engineKind) (engineKind, bool) {
	name := imageName(container.Image)
	isVLLM := strings.Contains(name, string(engineVLLM))
	isSGLang := strings.Contains(name, string(engineSGLang))
	switch {
	case isVLLM && isSGLang:
		// Both names in one image, so the arguments to use are unknown.
		return "", false
	case isVLLM:
		return engineVLLM, true
	case isSGLang:
		return engineSGLang, true
	}
	if container.Name == kserveModelContainerName && (declared == engineVLLM || declared == engineSGLang) {
		return declared, true
	}
	return "", false
}

// imageName returns the last path element of an image reference with the tag or digest
// removed, so that "vllm/vllm-openai:v0.29.0" gives "vllm-openai". The registry and the
// organization are left out on purpose: an unrelated image published beside an engine, such
// as vllm/router, must not be taken for the engine itself.
func imageName(image string) string {
	ref := strings.ToLower(image)
	if at := strings.Index(ref, "@"); at > -1 {
		ref = ref[:at]
	}
	ref = path.Base(ref)
	if colon := strings.LastIndex(ref, ":"); colon > -1 {
		ref = ref[:colon]
	}
	return ref
}

// engineEndpointHostPort drops a scheme, so that one annotation value serves both engines.
// vLLM takes a URL and SGLang takes a bare host and port.
func engineEndpointHostPort(endpoint string) string {
	if i := strings.Index(endpoint, "://"); i > -1 {
		endpoint = endpoint[i+3:]
	}
	return strings.TrimSuffix(endpoint, "/")
}

// engineSkipReason names the reason a container's command line must be left alone, and
// returns an empty string when the flag can be appended.
func engineSkipReason(container corev1.Container) string {
	if hasFlag(container.Command, engineTracesEndpointFlag) || hasFlag(container.Args, engineTracesEndpointFlag) {
		return "the pod spec already sets " + engineTracesEndpointFlag
	}
	if len(container.Command) > 0 {
		if base := path.Base(container.Command[0]); slices.Contains(engineShells, base) {
			return "the command runs a shell, which would take the flag as its own argument"
		}
	}
	// With no command and no args, the command line comes from the image. A non-empty args
	// would replace the image's CMD, which can remove the server's own arguments.
	if len(container.Command) == 0 && len(container.Args) == 0 {
		return "the pod spec sets no command and no args, so the image supplies them"
	}
	return ""
}

func injectEngineTracesConfig(cfg engineTracesConfig, kind engineKind, pod corev1.Pod, index int, envs []corev1.EnvVar) corev1.Pod {
	container := &pod.Spec.Containers[index]

	switch kind {
	case engineVLLM:
		// The exporter keeps only the host and port and is built insecure, so the scheme
		// selects TLS. A plaintext receiver needs grpc:// rather than https://.
		container.Args = append(container.Args, fmt.Sprintf("%s=%s://%s", engineTracesEndpointFlag, engineTracesProtocol, cfg.endpoint))
	case engineSGLang:
		if !hasFlag(container.Command, sglangEnableTraceFlag) && !hasFlag(container.Args, sglangEnableTraceFlag) {
			container.Args = append(container.Args, sglangEnableTraceFlag)
		}
		container.Args = append(container.Args, fmt.Sprintf("%s=%s", engineTracesEndpointFlag, cfg.endpoint))
		// SGLang names itself "sglang" when nothing else is set, so every SGLang workload
		// would arrive under one service name. vLLM sets no service name, and the agent
		// pipeline derives one, so vLLM is left alone here.
		if name := engineServiceName(pod, index); name != "" && getIndexOfEnv(envs, constants.EnvOTELServiceName) == -1 {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  constants.EnvOTELServiceName,
				Value: name,
			})
		}
	default:
		return pod
	}

	if getIndexOfEnv(envs, envOtelExporterOTLPTracesProtocol) == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  envOtelExporterOTLPTracesProtocol,
			Value: engineTracesProtocol,
		})
	}

	// Both engines resolve every name in OTEL_PROPAGATORS while importing, and their images
	// carry no xray propagator, so a name they cannot resolve ends the process before the
	// server starts. Claim the variable with names the images do carry.
	if getIndexOfEnv(envs, constants.EnvOTELPropagators) == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvOTELPropagators,
			Value: engineTracesPropagators,
		})
	}

	if cfg.sampler != "" && getIndexOfEnv(envs, constants.EnvOTELTracesSampler) == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvOTELTracesSampler,
			Value: cfg.sampler,
		})
		if cfg.samplerArg != "" && getIndexOfEnv(envs, constants.EnvOTELTracesSamplerArg) == -1 {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  constants.EnvOTELTracesSamplerArg,
				Value: cfg.samplerArg,
			})
		}
	}

	return pod
}

// engineServiceName names the workload from the pod itself, with no API call: the KServe
// InferenceService label, then app.kubernetes.io/name, then the owning Deployment, which is
// the ReplicaSet name without its pod-template hash. It returns an empty string when the pod
// carries none of these, and the engine's own default then applies.
func engineServiceName(pod corev1.Pod, index int) string {
	if name := pod.Labels[kserveInferenceService]; name != "" {
		return name
	}
	if name := pod.Labels["app.kubernetes.io/name"]; name != "" {
		return name
	}
	for _, owner := range pod.OwnerReferences {
		if !strings.EqualFold(owner.Kind, "replicaset") {
			continue
		}
		if dash := strings.LastIndex(owner.Name, "-"); dash > 0 {
			return owner.Name[:dash]
		}
		return owner.Name
	}
	return ""
}

// hasFlag reports whether argv already carries a flag, set either as one token with "=" or
// as a flag followed by its value.
func hasFlag(argv []string, flag string) bool {
	for _, arg := range argv {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
