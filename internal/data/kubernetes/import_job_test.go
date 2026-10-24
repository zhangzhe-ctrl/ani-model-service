package kubernetes

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestBuildImportJob(t *testing.T) {
	backoff := int32(1)
	spec := ImportJobSpec{
		Namespace:             "ani-model",
		Name:                  "import-task-1",
		Image:                 "registry.example/model-importer:latest",
		Command:               []string{"/bin/import"},
		Args:                  []string{"--task", "task-1"},
		Env:                   []corev1.EnvVar{{Name: "SOURCE", Value: "modelscope"}},
		Volumes:               []corev1.Volume{{Name: "staging", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		VolumeMounts:          []corev1.VolumeMount{{Name: "staging", MountPath: "/staging"}},
		ServiceAccountName:    "model-importer",
		Labels:                map[string]string{"ani.example/task-id": "task-1"},
		Annotations:           map[string]string{"ani.example/attempt": "1"},
		BackoffLimit:          &backoff,
		ActiveDeadlineSeconds: func() *int64 { v := int64(3600); return &v }(),
	}

	job, err := BuildImportJob(spec)
	if err != nil {
		t.Fatalf("BuildImportJob() error = %v", err)
	}
	if job.Namespace != spec.Namespace || job.Name != spec.Name {
		t.Fatalf("job identity = %s/%s, want %s/%s", job.Namespace, job.Name, spec.Namespace, spec.Name)
	}
	if job.Labels[ManagedLabel] != "true" || job.Labels["ani.example/task-id"] != "task-1" {
		t.Fatalf("job labels = %#v", job.Labels)
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %q, want Never", got)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Name != ImportContainerName || container.Image != spec.Image {
		t.Fatalf("container = %#v", container)
	}
	if len(container.Command) != 1 || container.Command[0] != "/bin/import" {
		t.Fatalf("command = %#v", container.Command)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 1 {
		t.Fatalf("backoff limit = %#v", job.Spec.BackoffLimit)
	}
}

func TestBuildImportJobMakesStagingWritableByNonRootImporter(t *testing.T) {
	job, err := BuildImportJob(ImportJobSpec{
		Namespace: "ani-model",
		Name:      "import-nonroot-staging",
		Image:     "registry.example/model-importer:nonroot",
		Volumes: []corev1.Volume{{Name: "staging", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "import-stage"},
		}}},
		VolumeMounts: []corev1.VolumeMount{{Name: "staging", MountPath: "/staging"}},
	})
	if err != nil {
		t.Fatalf("BuildImportJob() error = %v", err)
	}
	security := job.Spec.Template.Spec.SecurityContext
	if security == nil || security.FSGroup == nil || *security.FSGroup != 65532 {
		t.Fatalf("import staging group = %#v, want fsGroup 65532 for the nonroot importer", security)
	}
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		t.Fatalf("import worker must remain nonroot: %#v", security)
	}
	if security.FSGroupChangePolicy == nil || *security.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Fatalf("staging ownership policy = %#v, want OnRootMismatch", security.FSGroupChangePolicy)
	}
	if got := job.Spec.Template.Spec.Containers[0].VolumeMounts[0]; got.Name != "staging" || got.MountPath != "/staging" || got.ReadOnly {
		t.Fatalf("staging PVC mount changed: %#v", got)
	}
}

func TestImportJobNameUsesTaskAndAttempt(t *testing.T) {
	if got, want := ImportJobName("11111111-1111-1111-1111-111111111111", 3), "ani-import-11111111111111111111111111111111-a3"; got != want {
		t.Fatalf("ImportJobName() = %q, want %q", got, want)
	}
}

func TestImportJobClientSecretLifecycle(t *testing.T) {
	client := NewImportJobClient(k8sfake.NewSimpleClientset())
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ani-model",
			Name:      "import-task-1-upload",
			Labels:    map[string]string{"ani.example/task-id": "task-1"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"model.bin": []byte("model payload")},
	}

	created, err := client.CreateSecret(context.Background(), secret)
	if err != nil {
		t.Fatalf("CreateSecret() error = %v", err)
	}
	if got := string(created.Data["model.bin"]); got != "model payload" {
		t.Fatalf("created secret data = %q, want %q", got, "model payload")
	}

	if err := client.DeleteSecret(context.Background(), secret.Namespace, secret.Name); err != nil {
		t.Fatalf("DeleteSecret() error = %v", err)
	}
	_, err = client.client.CoreV1().Secrets(secret.Namespace).Get(context.Background(), secret.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("secret after DeleteSecret() error = %v, want not found", err)
	}
}

func TestImportJobClientCreateAndStatus(t *testing.T) {
	client := NewImportJobClient(k8sfake.NewSimpleClientset())
	job, err := client.Create(context.Background(), ImportJobSpec{
		Namespace: "ani-model",
		Name:      "import-task-1",
		Image:     "registry.example/model-importer:latest",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if job.Name != "import-task-1" {
		t.Fatalf("created job name = %q", job.Name)
	}

	job.Status.Active = 1
	if _, err := client.client.BatchV1().Jobs(job.Namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}
	status, err := client.Status(context.Background(), job.Namespace, job.Name)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Phase != JobRunning || status.Active != 1 {
		t.Fatalf("status = %#v", status)
	}

	job.Status.Active = 0
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed"}}
	if _, err := client.client.BatchV1().Jobs(job.Namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}
	status, err = client.Status(context.Background(), job.Namespace, job.Name)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Phase != JobSucceeded || status.Reason != "Completed" {
		t.Fatalf("completed status = %#v", status)
	}
}

func TestImportJobClientDeleteJobUsesForegroundPropagation(t *testing.T) {
	client := NewImportJobClient(k8sfake.NewSimpleClientset())
	job, err := client.Create(context.Background(), ImportJobSpec{
		Namespace: "ani-model",
		Name:      "import-task-1",
		Image:     "registry.example/model-importer:latest",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := client.DeleteJob(context.Background(), job.Namespace, job.Name); err != nil {
		t.Fatalf("DeleteJob() error = %v", err)
	}
	actions := client.client.(*k8sfake.Clientset).Actions()
	deleteAction, ok := actions[len(actions)-1].(k8stesting.DeleteAction)
	if !ok {
		t.Fatalf("last action = %T, want DeleteAction", actions[len(actions)-1])
	}
	if got := deleteAction.GetDeleteOptions().PropagationPolicy; got == nil || *got != metav1.DeletePropagationForeground {
		t.Fatalf("propagation policy = %v, want foreground", got)
	}
	if _, err := client.client.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job after DeleteJob() error = %v, want not found", err)
	}
}

func TestBuildImportJobRequiresIdentityAndImage(t *testing.T) {
	for name, spec := range map[string]ImportJobSpec{
		"namespace": {Name: "import-task-1", Image: "image"},
		"name":      {Namespace: "ani-model", Image: "image"},
		"image":     {Namespace: "ani-model", Name: "import-task-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildImportJob(spec); err == nil {
				t.Fatal("BuildImportJob() error = nil")
			}
		})
	}
}
