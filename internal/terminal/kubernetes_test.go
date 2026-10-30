package terminal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const ns = "terminals"

func newTestBackend(t *testing.T) (*Manager, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	k := NewKubernetes(client, Config{
		Namespace:          ns,
		Image:              "default-image",
		IdleTimeoutMinutes: 30,
		ProvisionTimeout:   5 * time.Second,
	})
	k.drv.(*kubeDriver).pollInterval = 5 * time.Millisecond
	return k, client
}

// becomeReady spielt das Kubelet: Sobald der Pod existiert, wird er Running und Ready.
func becomeReady(t *testing.T, client *fake.Clientset, name, ip string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		for ctx.Err() == nil {
			pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.PodIP = ip
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				_, _ = client.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	t.Cleanup(cancel)
}

func countActions(client *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func TestEnsureCreatesPod(t *testing.T) {
	k, client := newTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "dev", ContextID: "chat:42"}
	spec := Spec{
		"image":                "custom-image",
		"cpu_limit":            "500m",
		"memory_limit":         "1Gi",
		"storage":              "5Gi",
		"idle_timeout_minutes": float64(10),
		"env":                  map[string]any{"FOO": "bar", "OPEN_TERMINAL_API_KEY": "darf-nicht-durch"},
	}
	becomeReady(t, client, instanceName(key), "10.0.0.7")

	inst, err := k.Ensure(context.Background(), key, spec)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Host != "10.0.0.7" || inst.Port != Port || inst.APIKey == "" {
		t.Errorf("instance = %+v", inst)
	}

	secret, err := client.CoreV1().Secrets(ns).Get(context.Background(), secretName(inst.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.StringData[apiKeySecretKey] != inst.APIKey {
		t.Error("API-Key im Secret passt nicht zur Instanz")
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), inst.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("pvc fehlt: %v", err)
	}

	pod, _ := client.CoreV1().Pods(ns).Get(context.Background(), inst.Name, metav1.GetOptions{})
	c := pod.Spec.Containers[0]
	if c.Image != "custom-image" {
		t.Errorf("image = %q", c.Image)
	}
	if got := c.Resources.Limits.Cpu().String(); got != "500m" {
		t.Errorf("cpu limit = %s", got)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if ref := env["OPEN_TERMINAL_API_KEY"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != secretName(inst.Name) {
		t.Errorf("OPEN_TERMINAL_API_KEY nicht aus dem Secret: %+v", env["OPEN_TERMINAL_API_KEY"])
	}
	for name, want := range map[string]string{
		"FOO": "bar", "OPEN_TERMINAL_CPU_COUNT": "0.5", "OPEN_TERMINAL_MEMORY_BYTES": "1073741824",
	} {
		if env[name].Value != want {
			t.Errorf("env %s = %q, want %q", name, env[name].Value, want)
		}
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != homeDir {
		t.Errorf("volume mounts = %+v", c.VolumeMounts)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("Service-Account-Token darf nicht gemountet werden")
	}
	if keyOf(pod) != key || idleTimeoutOf(pod) != 10 {
		t.Errorf("annotations = %v", pod.Annotations)
	}

	// Zweiter Aufruf kommt aus dem Cache, ohne die API zu fragen.
	before := len(client.Actions())
	if again, err := k.Ensure(context.Background(), key, spec); err != nil || again != inst {
		t.Errorf("again = %+v, %v", again, err)
	}
	if len(client.Actions()) != before {
		t.Error("Cache-Treffer hat die Kubernetes-API aufgerufen")
	}
}

func TestEnsureConcurrentCreatesOnePod(t *testing.T) {
	k, client := newTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "default", ContextID: "default"}
	becomeReady(t, client, instanceName(key), "10.0.0.8")

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, err := k.Ensure(context.Background(), key, nil)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countActions(client, "create", "pods"); n != 1 {
		t.Errorf("%d Pods angelegt, want 1", n)
	}
}

func TestEnsureReusesRunningPod(t *testing.T) {
	k, client := newTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "default", ContextID: "default"}
	name := instanceName(key)
	_, _ = client.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(name), Namespace: ns},
		Data:       map[string][]byte{apiKeySecretKey: []byte("existing-key")},
	}, metav1.CreateOptions{})
	_, _ = client.CoreV1().Pods(ns).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.0.0.9",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}, metav1.CreateOptions{})
	client.ClearActions()

	inst, err := k.Ensure(context.Background(), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.APIKey != "existing-key" || inst.Host != "10.0.0.9" {
		t.Errorf("instance = %+v", inst)
	}
	if n := countActions(client, "create", "pods"); n != 0 {
		t.Errorf("%d Pods angelegt, want 0", n)
	}
}

func TestEnsureFailsFastOnImagePull(t *testing.T) {
	k, client := newTestBackend(t)
	key := Key{UserID: "u1", PolicyID: "default", ContextID: "default"}
	go func() {
		for {
			pod, err := client.CoreV1().Pods(ns).Get(context.Background(), instanceName(key), metav1.GetOptions{})
			if err == nil {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"},
				}}}
				_, _ = client.CoreV1().Pods(ns).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	start := time.Now()
	_, err := k.Ensure(context.Background(), key, Spec{"image": "gibt-es-nicht"})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("err = %v, want ImagePullBackOff", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Fehler kam erst nach dem Provision-Timeout")
	}
}

func TestEnsureRejectsInvalidResources(t *testing.T) {
	k, client := newTestBackend(t)
	_, err := k.Ensure(context.Background(), Key{UserID: "u1"}, Spec{"cpu_limit": "viel"})
	if err == nil || !strings.Contains(err.Error(), "cpu_limit") {
		t.Fatalf("err = %v", err)
	}
	if len(client.Actions()) != 1 { // nur das Get, nichts angelegt
		t.Errorf("actions = %v", client.Actions())
	}
}

func TestRestrictedSecurityContext(t *testing.T) {
	k, _ := newTestBackend(t)
	pod, err := k.drv.(*kubeDriver).podManifest(Key{}, "p", Spec{
		"restricted":                 true,
		"container_security_context": map[string]any{"runAsUser": float64(1000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0].SecurityContext
	if c == nil || c.AllowPrivilegeEscalation == nil || *c.AllowPrivilegeEscalation || c.RunAsUser == nil || *c.RunAsUser != 1000 {
		t.Errorf("container security = %+v", c)
	}
	if p := pod.Spec.SecurityContext; p == nil || p.SeccompProfile == nil || p.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security = %+v", p)
	}
}

// provisioned legt per Ensure eine bereite Instanz an.
func provisioned(t *testing.T, k *Manager, client *fake.Clientset, key Key, spec Spec) Instance {
	t.Helper()
	becomeReady(t, client, instanceName(key), "10.0.0.1")
	inst, err := k.Ensure(context.Background(), key, spec)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestRefresh(t *testing.T) {
	k, client := newTestBackend(t)
	a := Key{UserID: "a", PolicyID: "default", ContextID: "default"}
	b := Key{UserID: "b", PolicyID: "default", ContextID: "default"}
	instA := provisioned(t, k, client, a, Spec{"storage": "1Gi"})
	provisioned(t, k, client, b, nil)

	// Gerade benutzt: only_idle überspringt die Instanz.
	user := "a"
	res, err := k.Refresh(context.Background(), Filter{UserID: &user}, true, false)
	if err != nil || res != (RefreshResult{Matched: 1, SkippedActive: 1}) {
		t.Fatalf("only_idle = %+v, %v", res, err)
	}

	res, err = k.Refresh(context.Background(), Filter{UserID: &user}, false, true)
	if err != nil || res != (RefreshResult{Matched: 1, Refreshed: 1, Reset: 1}) {
		t.Fatalf("refresh = %+v, %v", res, err)
	}
	for _, get := range []func() error{
		func() error {
			_, err := client.CoreV1().Pods(ns).Get(context.Background(), instA.Name, metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := client.CoreV1().Secrets(ns).Get(context.Background(), secretName(instA.Name), metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := client.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), instA.Name, metav1.GetOptions{})
			return err
		},
	} {
		if err := get(); !apierrors.IsNotFound(err) {
			t.Errorf("Objekt von a existiert noch: %v", err)
		}
	}

	infos, err := k.List(context.Background())
	if err != nil || len(infos) != 1 || infos[0].UserID != "b" || infos[0].Status != "running" {
		t.Errorf("list = %+v, %v", infos, err)
	}
}

func TestReapRemovesIdleInstances(t *testing.T) {
	k, client := newTestBackend(t)
	idle := Key{UserID: "idle", PolicyID: "default", ContextID: "default"}
	busy := Key{UserID: "busy", PolicyID: "default", ContextID: "default"}
	never := Key{UserID: "never", PolicyID: "default", ContextID: "default"}
	provisioned(t, k, client, idle, Spec{"idle_timeout_minutes": float64(1)})
	provisioned(t, k, client, busy, Spec{"idle_timeout_minutes": float64(1)})
	provisioned(t, k, client, never, Spec{"idle_timeout_minutes": float64(0)})

	release := k.Connected(busy)
	defer release()
	k.mu.Lock()
	for _, t := range k.instances {
		t.lastActive = time.Now().Add(-2 * time.Minute)
	}
	k.mu.Unlock()

	k.reap(context.Background())

	infos, _ := k.List(context.Background())
	var left []string
	for _, i := range infos {
		left = append(left, i.UserID)
	}
	if strings.Join(left, ",") != "busy,never" && strings.Join(left, ",") != "never,busy" {
		t.Errorf("übrig = %v, want busy und never", left)
	}
}

func TestPodNameIsStableAndValid(t *testing.T) {
	key := Key{UserID: "82bf3e1c-934c-4e59-bc69-2490680d3218", PolicyID: "Dev Policy", ContextID: "chat:abc"}
	if instanceName(key) != instanceName(key) {
		t.Fatal("nicht deterministisch")
	}
	if n := instanceName(key); len(n) > 63 || strings.ToLower(n) != n {
		t.Errorf("kein gültiger DNS-Name: %q", n)
	}
	if instanceName(key) == instanceName(Key{UserID: key.UserID, PolicyID: key.PolicyID}) {
		t.Error("Context fließt nicht in den Namen ein")
	}
}
