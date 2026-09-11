// Package buildkube 把 Orbit-DevOps 构建语义翻译为受限 Kubernetes Job。
package buildkube

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	ManagedByLabel        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "orbit-devops"
	BuildIDLabel          = "orbit-devops.dev/build-id"
	BuildOperationIDLabel = "orbit-devops.dev/build-operation-id"
	BuildAttemptIDLabel   = "orbit-devops.dev/build-attempt-id"
	ProjectIDLabel        = "orbit-devops.dev/project-id"
	ApplicationIDLabel    = "orbit-devops.dev/application-id"
	InputDigestAnnotation = "orbit-devops.dev/build-input-digest"
	buildContainerName    = "buildkit"
)

var pinnedImagePattern = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

type Config struct {
	Namespace               string
	FieldManager            string
	GitImage                string
	BuildkitImage           string
	RegistrySecretName      string
	RegistryInsecure        bool
	DockerHubMirror         string
	DockerHubMirrorInsecure bool
	ActiveDeadline          time.Duration
	TTL                     time.Duration
	CPU                     string
	Memory                  string
	PollInterval            time.Duration
}

type Adapter struct {
	client kubernetes.Interface
	config Config
	cpu    resource.Quantity
	memory resource.Quantity
}

func New(client kubernetes.Interface, config Config) (*Adapter, error) {
	if client == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	if strings.TrimSpace(config.Namespace) == "" || strings.TrimSpace(config.FieldManager) == "" ||
		!pinnedImagePattern.MatchString(config.GitImage) || !pinnedImagePattern.MatchString(config.BuildkitImage) ||
		config.ActiveDeadline <= 0 || config.TTL <= 0 {
		return nil, errors.New("invalid Kubernetes build configuration")
	}
	cpu, err := resource.ParseQuantity(config.CPU)
	if err != nil || cpu.Sign() <= 0 {
		return nil, errors.New("invalid build CPU limit")
	}
	memory, err := resource.ParseQuantity(config.Memory)
	if err != nil || memory.Sign() <= 0 {
		return nil, errors.New("invalid build memory limit")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	return &Adapter{client: client, config: config, cpu: cpu, memory: memory}, nil
}

// RenderJob 确定性生成一个 Attempt 对应的 Job；请求方不能覆盖镜像、资源或 Secret。
func (a *Adapter) RenderJob(execution buildworker.BuildExecution) *batchv1.Job {
	name := ResourceName(execution.BuildAttemptID)
	labels := ownershipLabels(execution)
	backoffLimit := int32(0)
	deadline := int64(a.config.ActiveDeadline / time.Second)
	ttl := int32(a.config.TTL / time.Second)
	automount := false
	runAsUser, runAsGroup := int64(1000), int64(1000)
	runAsNonRoot, privileged, allowEscalation := true, false, true
	readOnlyRoot := false
	fsGroup := int64(1000)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: a.cpu, corev1.ResourceMemory: a.memory},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: a.cpu, corev1.ResourceMemory: a.memory},
	}
	initContainer := corev1.Container{
		Name: "source", Image: a.config.GitImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"/bin/sh", "-ec"}, Args: []string{fetchScript},
		Env: []corev1.EnvVar{
			{Name: "REPOSITORY_URL", Value: execution.RepositoryURL}, {Name: "SOURCE_COMMIT", Value: execution.SourceCommit},
			// EmptyDir 的挂载点归 root 所有；仅为固定工作目录解除 Git 的所有者保护。
			{Name: "GIT_CONFIG_COUNT", Value: "1"}, {Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
			{Name: "GIT_CONFIG_VALUE_0", Value: "/workspace"},
		},
		SecurityContext: &corev1.SecurityContext{RunAsUser: &runAsUser, RunAsGroup: &runAsGroup,
			RunAsNonRoot: &runAsNonRoot, AllowPrivilegeEscalation: boolPointer(false),
			ReadOnlyRootFilesystem: &readOnlyRoot, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Resources: resources, VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
	}
	container := corev1.Container{
		Name: buildContainerName, Image: a.config.BuildkitImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"/bin/sh", "-ec"}, Args: []string{buildScript},
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/home/user"}, {Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
			{Name: "CONTEXT_PATH", Value: "/workspace/" + execution.ContextPath},
			{Name: "DOCKERFILE_PATH", Value: "/workspace/" + path.Dir(execution.DockerfilePath)},
			{Name: "DOCKERFILE_NAME", Value: path.Base(execution.DockerfilePath)},
			{Name: "PLATFORM", Value: execution.Platform},
			{Name: "DESTINATION", Value: execution.DestinationRepository + ":build-" + execution.BuildID.String()},
			{Name: "DESTINATION_REPOSITORY", Value: execution.DestinationRepository},
			{Name: "REGISTRY_HOST", Value: strings.SplitN(execution.DestinationRepository, "/", 2)[0]},
			{Name: "REGISTRY_INSECURE", Value: boolString(a.config.RegistryInsecure)},
			{Name: "DOCKERHUB_MIRROR", Value: strings.TrimSpace(a.config.DockerHubMirror)},
			{Name: "DOCKERHUB_MIRROR_INSECURE", Value: boolString(a.config.DockerHubMirrorInsecure)},
		},
		SecurityContext: &corev1.SecurityContext{RunAsUser: &runAsUser, RunAsGroup: &runAsGroup,
			RunAsNonRoot: &runAsNonRoot, Privileged: &privileged, AllowPrivilegeEscalation: &allowEscalation,
			// rootlesskit 依赖镜像内受控的 newuidmap/newgidmap setuid 路径，不能在这里 drop ALL。
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}},
		Resources: resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", ReadOnly: true},
			{Name: "buildkit-state", MountPath: "/home/user/.local/share/buildkit"},
		},
		TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "buildkit-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if a.config.RegistrySecretName != "" {
		mode := int32(0o400)
		volumes = append(volumes, corev1.Volume{Name: "registry-auth", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: a.config.RegistrySecretName, DefaultMode: &mode,
				Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json", Mode: &mode}}},
		}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "registry-auth", MountPath: "/home/user/.docker", ReadOnly: true})
		container.Env = append(container.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/home/user/.docker"})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.config.Namespace, Labels: labels,
			Annotations: map[string]string{InputDigestAnnotation: execution.InputDigest}},
		Spec: batchv1.JobSpec{BackoffLimit: &backoffLimit, ActiveDeadlineSeconds: &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels,
				Annotations: map[string]string{InputDigestAnnotation: execution.InputDigest}},
				Spec: corev1.PodSpec{ServiceAccountName: "orbit-devops-build-executor", AutomountServiceAccountToken: &automount,
					RestartPolicy: corev1.RestartPolicyNever, SecurityContext: &corev1.PodSecurityContext{FSGroup: &fsGroup},
					InitContainers: []corev1.Container{initContainer}, Containers: []corev1.Container{container}, Volumes: volumes}}},
	}
}

// ResourceName 让同一个 BuildAttempt 在 Worker 重启或消息重投后仍映射到同一个 Job。
func ResourceName(attemptID uuid.UUID) string {
	return "orbit-devops-build-" + attemptID.String()
}

func ownershipLabels(execution buildworker.BuildExecution) map[string]string {
	return map[string]string{ManagedByLabel: ManagedByValue, BuildIDLabel: execution.BuildID.String(),
		BuildOperationIDLabel: execution.BuildOperationID.String(), BuildAttemptIDLabel: execution.BuildAttemptID.String(),
		ProjectIDLabel: execution.ProjectID.String(), ApplicationIDLabel: execution.ApplicationID.String()}
}

func boolPointer(value bool) *bool { return &value }

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

const fetchScript = `set -eu
git init /workspace
git -C /workspace remote add origin "${REPOSITORY_URL}"
git -C /workspace fetch --depth=1 origin "${SOURCE_COMMIT}"
git -C /workspace checkout --detach FETCH_HEAD
test "$(git -C /workspace rev-parse HEAD)" = "${SOURCE_COMMIT}"
rm -rf /workspace/.git
`

const buildScript = `set -eu
if [ "${REGISTRY_INSECURE}" = "true" ]; then
  mkdir -p "${HOME}/.config/buildkit"
  printf '[registry."%s"]\n  http = true\n  insecure = true\n' "${REGISTRY_HOST}" > "${HOME}/.config/buildkit/buildkitd.toml"
fi
if [ -n "${DOCKERHUB_MIRROR}" ]; then
  mkdir -p "${HOME}/.config/buildkit"
  printf '[registry."docker.io"]\n  mirrors = ["%s"]\n' "${DOCKERHUB_MIRROR}" >> "${HOME}/.config/buildkit/buildkitd.toml"
  if [ "${DOCKERHUB_MIRROR_INSECURE}" = "true" ] && { [ "${DOCKERHUB_MIRROR}" != "${REGISTRY_HOST}" ] || [ "${REGISTRY_INSECURE}" != "true" ]; }; then
    printf '[registry."%s"]\n  http = true\n  insecure = true\n' "${DOCKERHUB_MIRROR}" >> "${HOME}/.config/buildkit/buildkitd.toml"
  fi
fi
metadata="$(mktemp)"
build_log="$(mktemp)"
set +e
buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local "context=${CONTEXT_PATH}" \
  --local "dockerfile=${DOCKERFILE_PATH}" \
  --opt "filename=${DOCKERFILE_NAME}" \
  --opt "platform=${PLATFORM}" \
  --output "type=image,name=${DESTINATION},push=true" \
  --metadata-file "${metadata}" > "${build_log}" 2>&1
status="$?"
set -e
cat "${build_log}"
if [ "${status}" -ne 0 ]; then
  error_code="dockerfile_build_failed"
  if grep -Eiq 'failed to push|failed to do request|connection refused|no such host|unexpected status|server gave HTTP response' "${build_log}"; then
    error_code="registry_unavailable"
  fi
  printf '{"error_code":"%s"}' "${error_code}" > /dev/termination-log
  exit "${status}"
fi
compact="$(tr -d '\n ' < "${metadata}")"
digest="$(printf '%s' "${compact}" | sed -n 's/.*"containerimage.digest":"\(sha256:[a-f0-9]\{64\}\)".*/\1/p')"
test -n "${digest}"
printf '{"repository":"%s","digest":"%s"}' "${DESTINATION_REPOSITORY}" "${digest}" > /dev/termination-log
`
