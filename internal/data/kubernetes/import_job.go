package kubernetes

import (
	"context"
	"errors"
	"fmt"
	workbiz "github.com/liangzai006/ani-model-service/internal/biz/work"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
)

const (
	// ManagedLabel identifies Jobs owned by the model import controller.
	ManagedLabel = "ani.liangzai006.io/import-job"
	// ImportContainerName is stable so logs and pod selectors do not depend on
	// the container image name.
	ImportContainerName = "import"
)

var ErrKubernetesClientUnavailable = errors.New("kubernetes client unavailable")

// ImportJobSpec contains only the fields needed to construct a one-shot import
// Job. Lifecycle orchestration and task persistence remain outside this adapter.
type ImportJobSpec struct {
	Namespace    string
	Name         string
	Image        string
	Command      []string
	Args         []string
	Env          []corev1.EnvVar
	EnvFrom      []corev1.EnvFromSource
	Volumes      []corev1.Volume
	VolumeMounts []corev1.VolumeMount

	ServiceAccountName string
	Labels             map[string]string
	Annotations        map[string]string

	BackoffLimit            *int32
	ActiveDeadlineSeconds   *int64
	TTLSecondsAfterFinished *int32
}

// BuildImportJob builds a typed batch/v1 Job manifest without contacting the
// API server. The caller can inspect or further decorate the manifest before
// submitting it through ImportJobClient.
func BuildImportJob(spec ImportJobSpec) (*batchv1.Job, error) {
	if spec.Namespace == "" {
		return nil, errors.New("import job namespace is required")
	}
	if spec.Name == "" {
		return nil, errors.New("import job name is required")
	}
	if spec.Image == "" {
		return nil, errors.New("import job image is required")
	}

	labels := cloneStringMap(spec.Labels)
	labels[ManagedLabel] = "true"
	annotations := cloneStringMap(spec.Annotations)
	// The importer image runs as nonroot (UID 65532). Fresh staging PVCs
	// otherwise belong to root:root with mode 0755. Give the worker the
	// dedicated volume group without overriding the image's process UID/GID.
	nonRoot := true
	stagingGroup := int64(65532)
	groupChangePolicy := corev1.FSGroupChangeOnRootMismatch

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   spec.Namespace,
			Name:        spec.Name,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            spec.BackoffLimit,
			ActiveDeadlineSeconds:   spec.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: spec.TTLSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: cloneStringMap(labels)},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					Volumes:            cloneVolumes(spec.Volumes),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        &nonRoot,
						FSGroup:             &stagingGroup,
						FSGroupChangePolicy: &groupChangePolicy,
					},
					Containers: []corev1.Container{{
						Name:         ImportContainerName,
						Image:        spec.Image,
						Command:      append([]string(nil), spec.Command...),
						Args:         append([]string(nil), spec.Args...),
						Env:          cloneEnv(spec.Env),
						EnvFrom:      cloneEnvFrom(spec.EnvFrom),
						VolumeMounts: cloneVolumeMounts(spec.VolumeMounts),
					}},
				},
			},
		},
	}, nil
}

// ImportJobClient owns only the Kubernetes API calls for import Jobs. It does
// not claim tasks, run providers, or update model version state.
type ImportJobClient struct {
	client k8sclient.Interface
}

func NewImportJobClient(client k8sclient.Interface) *ImportJobClient {
	return &ImportJobClient{client: client}
}

func (c *ImportJobClient) Create(ctx context.Context, spec ImportJobSpec) (*batchv1.Job, error) {
	job, err := BuildImportJob(spec)
	if err != nil {
		return nil, err
	}
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	created, err := c.client.BatchV1().Jobs(spec.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create import job %s/%s: %w", spec.Namespace, spec.Name, err)
	}
	return created, nil
}

func (c *ImportJobClient) Get(ctx context.Context, namespace, name string) (*batchv1.Job, error) {
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	job, err := c.client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get import job %s/%s: %w", namespace, name, err)
	}
	return job, nil
}

func (c *ImportJobClient) Status(ctx context.Context, namespace, name string) (ImportJobStatus, error) {
	job, err := c.Get(ctx, namespace, name)
	if err != nil {
		return ImportJobStatus{}, err
	}
	return JobStatus(job), nil
}

// DeleteJob deletes an import Job and its dependent Pods through foreground
// garbage collection. The foreground policy keeps the Job's dependents tied
// to the lifecycle cleanup, which allows callers to clean up mounted storage
// after requesting Job deletion.
func (c *ImportJobClient) DeleteJob(ctx context.Context, namespace, name string) error {
	if c == nil || c.client == nil {
		return ErrKubernetesClientUnavailable
	}
	propagation := metav1.DeletePropagationForeground
	if err := c.client.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
		return fmt.Errorf("delete import job %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *ImportJobClient) CreatePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	if pvc == nil || pvc.Namespace == "" || pvc.Name == "" {
		return nil, errors.New("import pvc identity is required")
	}
	created, err := c.client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create import pvc %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}
	return created, nil
}

func (c *ImportJobClient) GetPVC(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	pvc, err := c.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get import pvc %s/%s: %w", namespace, name, err)
	}
	return pvc, nil
}

// DeletePVC deletes a PersistentVolumeClaim. This is best-effort cleanup after job completion.
func (c *ImportJobClient) DeletePVC(ctx context.Context, namespace, name string) error {
	if c == nil || c.client == nil {
		return ErrKubernetesClientUnavailable
	}
	err := c.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete import pvc %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *ImportJobClient) CreateSecret(ctx context.Context, secret *corev1.Secret) (*corev1.Secret, error) {
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	if secret == nil || secret.Namespace == "" || secret.Name == "" {
		return nil, errors.New("import secret identity is required")
	}
	created, err := c.client.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create import secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return created, nil
}

func (c *ImportJobClient) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	if c == nil || c.client == nil {
		return nil, ErrKubernetesClientUnavailable
	}
	secret, err := c.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get import secret %s/%s: %w", namespace, name, err)
	}
	return secret, nil
}

func (c *ImportJobClient) DeleteSecret(ctx context.Context, namespace, name string) error {
	if c == nil || c.client == nil {
		return ErrKubernetesClientUnavailable
	}
	err := c.client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete import secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *ImportJobClient) Logs(ctx context.Context, namespace, name string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrKubernetesClientUnavailable
	}
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return "", fmt.Errorf("list import job pods %s/%s: %w", namespace, name, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("import job %s/%s has no pod", namespace, name)
	}
	container := ImportContainerName
	// Provider CLIs may emit a large progress log. The controller only needs
	// the stable result line at the end, so request the tail rather than the
	// first bytes of the log stream.
	tailLines := int64(200)
	stream, err := c.client.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: container, TailLines: &tailLines}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read import job logs %s/%s: %w", namespace, name, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read import job logs %s/%s: %w", namespace, name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ImportJobName returns the deterministic Job name used for an import attempt.
func ImportJobName(taskID string, attempt int) string {
	return workbiz.ImportJobName(taskID, attempt)
}

type JobPhase string

const (
	JobPending   JobPhase = "pending"
	JobRunning   JobPhase = "running"
	JobSucceeded JobPhase = "succeeded"
	JobFailed    JobPhase = "failed"
)

type ImportJobStatus struct {
	Phase     JobPhase
	Active    int32
	Succeeded int32
	Failed    int32
	Reason    string
	Message   string
	StartTime *time.Time
	EndTime   *time.Time
}

func JobStatus(job *batchv1.Job) ImportJobStatus {
	if job == nil {
		return ImportJobStatus{}
	}
	status := ImportJobStatus{
		Phase:     JobPending,
		Active:    job.Status.Active,
		Succeeded: job.Status.Succeeded,
		Failed:    job.Status.Failed,
	}
	if job.Status.StartTime != nil {
		started := job.Status.StartTime.Time
		status.StartTime = &started
	}
	if job.Status.CompletionTime != nil {
		completed := job.Status.CompletionTime.Time
		status.EndTime = &completed
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		status.Reason, status.Message = condition.Reason, condition.Message
		switch condition.Type {
		case batchv1.JobComplete:
			status.Phase = JobSucceeded
		case batchv1.JobFailed:
			status.Phase = JobFailed
		}
	}
	if status.Phase == JobPending {
		switch {
		case status.Active > 0:
			status.Phase = JobRunning
		case status.Failed > 0:
			status.Phase = JobFailed
		case status.Succeeded > 0:
			status.Phase = JobSucceeded
		}
	}
	return status
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneEnv(in []corev1.EnvVar) []corev1.EnvVar {
	return append([]corev1.EnvVar(nil), in...)
}

func cloneEnvFrom(in []corev1.EnvFromSource) []corev1.EnvFromSource {
	return append([]corev1.EnvFromSource(nil), in...)
}

func cloneVolumes(in []corev1.Volume) []corev1.Volume {
	return append([]corev1.Volume(nil), in...)
}

func cloneVolumeMounts(in []corev1.VolumeMount) []corev1.VolumeMount {
	return append([]corev1.VolumeMount(nil), in...)
}
