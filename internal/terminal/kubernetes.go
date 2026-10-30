package terminal

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "chatbox"
	nameLabel      = "app.kubernetes.io/name"

	// Labels erlauben weder beliebige Länge noch ":" (chat:<id>), deshalb stehen die
	// Rohwerte in Annotationen.
	userAnnotation    = "chatbox/user-id"
	policyAnnotation  = "chatbox/policy-id"
	contextAnnotation = "chatbox/context-id"
	idleAnnotation    = "chatbox/idle-timeout-minutes"

	apiKeySecretKey = "api-key"
)

// Wie RESTRICTED_*_SECURITY_CONTEXT in terminals, für Policies mit restricted: true.
var (
	restrictedPodSecurity = map[string]any{
		"runAsNonRoot":   true,
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
	restrictedContainerSecurity = map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"runAsNonRoot":             true,
	}
)

// Gründe, aus denen ein wartender Container nie bereit wird.
var fatalWaitingReasons = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError"}

// NewKubernetes legt pro Key einen Pod mit Secret (API-Key) und optional einem PVC an.
// Die Instanz wird direkt über die Pod-IP angesprochen, chatbox muss also im Cluster laufen.
func NewKubernetes(client kubernetes.Interface, cfg Config) *Manager {
	cfg.Backend = "kubernetes"
	return newManager(&kubeDriver{client: client, cfg: cfg, pollInterval: time.Second}, cfg)
}

type kubeDriver struct {
	client       kubernetes.Interface
	cfg          Config
	pollInterval time.Duration
}

func (k *kubeDriver) provision(ctx context.Context, key Key, name string, spec Spec) (Instance, error) {
	pods := k.client.CoreV1().Pods(k.cfg.Namespace)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		pod = nil
	case err != nil:
		return Instance{}, fmt.Errorf("pod %s lesen: %w", name, err)
	case !running(pod):
		// Wird gerade gelöscht oder ist beendet: Löschen abwarten und neu anlegen.
		if err := k.deletePodAndWait(ctx, name); err != nil {
			return Instance{}, err
		}
		pod = nil
	}

	var apiKey string
	if pod == nil {
		slog.Info("provisioning pod", "pod", name, "user_id", key.UserID, "policy_id", key.PolicyID, "context_id", key.ContextID)
		apiKey, err = k.create(ctx, key, name, spec)
	} else {
		apiKey, err = k.readAPIKey(ctx, name)
	}
	if err != nil {
		return Instance{}, err
	}

	ip, err := k.waitReady(ctx, name)
	if err != nil {
		return Instance{}, err
	}
	return Instance{Name: name, Host: ip, Port: Port, APIKey: apiKey}, nil
}

func (k *kubeDriver) list(ctx context.Context) ([]state, error) {
	list, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("pods auflisten: %w", err)
	}
	states := make([]state, 0, len(list.Items))
	for _, pod := range list.Items {
		states = append(states, state{
			key:                keyOf(&pod),
			uid:                string(pod.UID),
			name:               pod.Name,
			host:               pod.Status.PodIP,
			running:            running(&pod),
			idleTimeoutMinutes: idleTimeoutOf(&pod),
		})
	}
	return states, nil
}

// remove löscht Pod und Secret, mit reset auch das PVC (Daten des Users).
func (k *kubeDriver) remove(ctx context.Context, name string, reset bool) error {
	core := k.client.CoreV1()
	ignoreMissing := func(err error) error {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := ignoreMissing(core.Pods(k.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("pod %s löschen: %w", name, err)
	}
	if err := ignoreMissing(core.Secrets(k.cfg.Namespace).Delete(ctx, secretName(name), metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("secret %s löschen: %w", secretName(name), err)
	}
	if reset {
		if err := ignoreMissing(core.PersistentVolumeClaims(k.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return fmt.Errorf("pvc %s löschen: %w", name, err)
		}
	}
	return nil
}

func (k *kubeDriver) create(ctx context.Context, key Key, name string, spec Spec) (string, error) {
	pod, err := k.podManifest(key, name, spec)
	if err != nil {
		return "", err
	}
	apiKey := rand.Text()

	secret := &corev1.Secret{
		ObjectMeta: k.meta(key, secretName(name), spec),
		StringData: map[string]string{apiKeySecretKey: apiKey},
	}
	secrets := k.client.CoreV1().Secrets(k.cfg.Namespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		// Übrig von einem gelöschten Pod: neuen Key setzen.
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return "", fmt.Errorf("secret %s aktualisieren: %w", secret.Name, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("secret %s anlegen: %w", secret.Name, err)
	}

	if size := str(spec, "storage"); size != "" {
		if err := k.ensurePVC(ctx, key, name, size, spec); err != nil {
			return "", err
		}
	}

	if _, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("pod %s anlegen: %w", name, err)
	}
	return apiKey, nil
}

func (k *kubeDriver) ensurePVC(ctx context.Context, key Key, name, size string, spec Spec) error {
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("storage %q: %w", size, err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: k.meta(key, name, spec),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: quantity},
			},
		},
	}
	if k.cfg.StorageClass != "" {
		pvc.Spec.StorageClassName = &k.cfg.StorageClass
	}
	_, err = k.client.CoreV1().PersistentVolumeClaims(k.cfg.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("pvc %s anlegen: %w", name, err)
	}
	return nil
}

func (k *kubeDriver) podManifest(key Key, name string, spec Spec) (*corev1.Pod, error) {
	extraEnv, lim, err := containerEnv(spec)
	if err != nil {
		return nil, err
	}
	env := []corev1.EnvVar{{
		Name: "OPEN_TERMINAL_API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName(name)},
			Key:                  apiKeySecretKey,
		}},
	}}
	for _, e := range extraEnv {
		env = append(env, corev1.EnvVar{Name: e.name, Value: e.value})
	}
	resources := corev1.ResourceList{}
	if lim.cpu != nil {
		resources[corev1.ResourceCPU] = *lim.cpu
	}
	if lim.memory != nil {
		resources[corev1.ResourceMemory] = *lim.memory
	}

	restricted, _ := spec["restricted"].(bool)
	podSecurity, err := securityContext[corev1.PodSecurityContext](restricted, restrictedPodSecurity, spec["pod_security_context"])
	if err != nil {
		return nil, fmt.Errorf("pod_security_context: %w", err)
	}
	containerSecurity, err := securityContext[corev1.SecurityContext](restricted, restrictedContainerSecurity, spec["container_security_context"])
	if err != nil {
		return nil, fmt.Errorf("container_security_context: %w", err)
	}
	if egressFiltered(spec) && containerSecurity == nil && !restricted {
		containerSecurity = &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
		}
	}

	container := corev1.Container{
		Name:            "open-terminal",
		Image:           cmp.Or(str(spec, "image"), k.cfg.Image),
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: Port}},
		Env:             env,
		Resources:       corev1.ResourceRequirements{Limits: resources},
		SecurityContext: containerSecurity,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromInt32(Port),
			}},
			PeriodSeconds: 2,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: k.meta(key, name, spec),
		Spec: corev1.PodSpec{
			Containers:      []corev1.Container{container},
			RestartPolicy:   corev1.RestartPolicyAlways,
			SecurityContext: podSecurity,
			// Der Terminal-User soll weder die Kubernetes-API noch Service-Variablen sehen.
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
		},
	}
	if str(spec, "storage") != "" {
		pod.Spec.Volumes = []corev1.Volume{{
			Name: "home",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: name,
			}},
		}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "home", MountPath: homeDir}}
	}
	return pod, nil
}

func (k *kubeDriver) meta(key Key, name string, spec Spec) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: k.cfg.Namespace,
		Labels: map[string]string{
			managedByLabel: managedBy,
			nameLabel:      "open-terminal",
		},
		Annotations: map[string]string{
			userAnnotation:    key.UserID,
			policyAnnotation:  key.PolicyID,
			contextAnnotation: key.ContextID,
			idleAnnotation:    strconv.Itoa(idleTimeout(k.cfg, spec)),
		},
	}
}

func (k *kubeDriver) readAPIKey(ctx context.Context, name string) (string, error) {
	secret, err := k.client.CoreV1().Secrets(k.cfg.Namespace).Get(ctx, secretName(name), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("secret %s lesen: %w", secretName(name), err)
	}
	if v := secret.Data[apiKeySecretKey]; len(v) > 0 {
		return string(v), nil
	}
	// Nur relevant für Fake-Clients, der API-Server wandelt StringData in Data.
	if v := secret.StringData[apiKeySecretKey]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("secret %s enthält keinen API-Key", secretName(name))
}

func (k *kubeDriver) waitReady(ctx context.Context, name string) (string, error) {
	pods := k.client.CoreV1().Pods(k.cfg.Namespace)
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return "", fmt.Errorf("pod %s ist beendet (%s)", name, pod.Status.Phase)
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && slices.Contains(fatalWaitingReasons, w.Reason) {
					return "", fmt.Errorf("pod %s startet nicht: %s: %s", name, w.Reason, w.Message)
				}
			}
			if ready(pod) && pod.Status.PodIP != "" {
				return pod.Status.PodIP, nil
			}
		} else if !apierrors.IsNotFound(err) {
			slog.Debug("waiting for pod", "pod", name, "err", err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("pod %s nicht rechtzeitig bereit: %w", name, ctx.Err())
		case <-time.After(k.pollInterval):
		}
	}
}

func (k *kubeDriver) deletePodAndWait(ctx context.Context, name string) error {
	pods := k.client.CoreV1().Pods(k.cfg.Namespace)
	if err := pods.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pod %s löschen: %w", name, err)
	}
	for {
		if _, err := pods.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pod %s nicht rechtzeitig gelöscht: %w", name, ctx.Err())
		case <-time.After(k.pollInterval):
		}
	}
}

// --- Helfer ---

func secretName(podName string) string {
	return podName + "-apikey"
}

func keyOf(pod *corev1.Pod) Key {
	return Key{
		UserID:    pod.Annotations[userAnnotation],
		PolicyID:  pod.Annotations[policyAnnotation],
		ContextID: pod.Annotations[contextAnnotation],
	}
}

func idleTimeoutOf(pod *corev1.Pod) int {
	n, _ := strconv.Atoi(pod.Annotations[idleAnnotation])
	return n
}

func running(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil &&
		(pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending || pod.Status.Phase == "")
}

func ready(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// securityContext mischt die Restricted-Vorgabe mit dem Wert aus der Policy und wandelt
// das Ergebnis in den Kubernetes-Typ. nil, wenn beides fehlt.
func securityContext[T any](restricted bool, preset map[string]any, override any) (*T, error) {
	merged := map[string]any{}
	if restricted {
		merged = deepMerge(merged, preset)
	}
	if m, ok := override.(map[string]any); ok {
		merged = deepMerge(merged, m)
	} else if override != nil {
		return nil, fmt.Errorf("muss ein Objekt sein")
	}
	if len(merged) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func deepMerge(base, override map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range override {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = deepMerge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}
