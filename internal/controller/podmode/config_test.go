package podmode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func fakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
}

func ref(name string) corev1.LocalObjectReference { return corev1.LocalObjectReference{Name: name} }

// configStandIn is a stand-in whose workload uses every Secret and
// ConfigMap form pod mode carries.
func configStandIn(t *testing.T) *corev1.Pod {
	t.Helper()
	in := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-1", Namespace: "tenant", UID: uid},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(podmode.RuntimeClassName),
			Volumes: []corev1.Volume{
				{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "tls", DefaultMode: ptr.To[int32](0o440)}}},
				{Name: "conf", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: ref("settings"),
					Items:                []corev1.KeyToPath{{Key: "app.yaml", Path: "cfg/app.yaml", Mode: ptr.To[int32](0o600)}},
				}}},
				{Name: "maybe", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "absent", Optional: ptr.To(true)}}},
			},
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "registry.example/app:1",
				EnvFrom: []corev1.EnvFromSource{
					{Prefix: "DB_", SecretRef: &corev1.SecretEnvSource{LocalObjectReference: ref("db")}},
					{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: ref("absent"), Optional: ptr.To(true)}},
				},
				Env: []corev1.EnvVar{
					{Name: "PLAIN", Value: "v"},
					{Name: "DB_user", Value: "overridden"},
					{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: ref("creds"), Key: "token"}}},
					{Name: "MODE", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: ref("settings"), Key: "mode"}}},
					{Name: "OPT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: ref("creds"), Key: "none", Optional: ptr.To(true)}}},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "tls", MountPath: "/etc/tls", ReadOnly: true},
					{Name: "conf", MountPath: "/etc/app/app.yaml", SubPath: "cfg/app.yaml"},
					{Name: "maybe", MountPath: "/etc/maybe"},
				},
			}},
		},
	}
	out, err := podmode.Rewrite(in, shim)
	if err != nil {
		t.Fatal(err)
	}
	out.Spec.NodeName = "node-a"
	return out
}

func tenantConfig() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "tenant"},
			Data: map[string][]byte{"user": []byte("admin"), "password": []byte("s3cret-db"), "bad-name": []byte("x")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "tenant"},
			Data: map[string][]byte{"token": []byte("s3cret-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "tenant"},
			Data: map[string][]byte{"tls.crt": []byte("CERT"), "tls.key": []byte("s3cret-key")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "tenant"},
			Data: map[string]string{"mode": "fast", "app.yaml": "a: 1"}},
		// The same names in another namespace are never read.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "other"},
			Data: map[string][]byte{"token": []byte("wrong")}},
	}
}

func TestSecretsAndConfigMapsReachTheMicroVM(t *testing.T) {
	r, c, _ := setup(t, append(tenantConfig(), configStandIn(t))...)
	reconcileOnce(t, r, "tenant", "sb-1")

	var copySecret corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &copySecret); err != nil {
		t.Fatal(err)
	}
	if copySecret.Labels[podmode.LabelUID] != uid || copySecret.Annotations[podmode.AnnotationStandIn] != "tenant/sb-1" {
		t.Errorf("copy metadata %v %v", copySecret.Labels, copySecret.Annotations)
	}
	val := func(sel *corev1.SecretKeySelector) string {
		t.Helper()
		if sel == nil || sel.Name != sbKey {
			t.Fatalf("not a reference to the copy: %+v", sel)
		}
		return string(copySecret.Data[sel.Key])
	}
	sb := getSandbox(t, c)
	env := map[string]string{}
	for _, e := range sb.Spec.Env {
		if e.ValueFrom == nil {
			env[e.Name] = "literal:" + e.Value
			continue
		}
		env[e.Name] = val(e.ValueFrom.SecretKeyRef)
	}
	want := map[string]string{
		"DB_user": "literal:overridden", "DB_password": "s3cret-db", "PLAIN": "literal:v",
		"TOKEN": "s3cret-token", "MODE": "fast", "DB_bad-name": "x",
	}
	if len(env) != len(want) {
		t.Errorf("env %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}

	files := map[string]string{}
	modes := map[string]int32{}
	for _, f := range sb.Spec.SecretFiles {
		for _, it := range f.Items {
			files[it.Path] = val(&corev1.SecretKeySelector{LocalObjectReference: ref(f.SecretName), Key: it.Key})
			modes[it.Path] = *it.Mode
		}
	}
	if len(files) != 3 || files["/etc/tls/tls.crt"] != "CERT" || files["/etc/tls/tls.key"] != "s3cret-key" ||
		files["/etc/app/app.yaml"] != "a: 1" {
		t.Errorf("files %v", files)
	}
	if modes["/etc/tls/tls.key"] != 0o440 || modes["/etc/app/app.yaml"] != 0o600 {
		t.Errorf("modes %v", modes)
	}
	// No value is in the SwiftSandbox itself, and nothing else is copied.
	raw, _ := json.Marshal(sb)
	for _, s := range []string{"s3cret", "fast", "CERT", "wrong"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("%q is in the SwiftSandbox", s)
		}
	}
	// db (3), creds/token, tls (2), settings (2).
	if len(copySecret.Data) != 8 {
		t.Errorf("copy holds %d keys, want 8", len(copySecret.Data))
	}
}

func TestMissingSecretHoldsTheMicroVM(t *testing.T) {
	for name, drop := range map[string]func([]client.Object) []client.Object{
		"secret":    func(o []client.Object) []client.Object { return append(o[:1:1], o[2:]...) },
		"configmap": func(o []client.Object) []client.Object { return append(o[:3:3], o[4:]...) },
		"key": func(o []client.Object) []client.Object {
			o[1] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "tenant"}}
			return o
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, rec := setup(t, append(drop(tenantConfig()), configStandIn(t))...)
			if res := reconcileOnce(t, r, "tenant", "sb-1"); res.RequeueAfter != configRequeue {
				t.Errorf("requeue %v", res.RequeueAfter)
			}
			var sb sandboxv1alpha1.SwiftSandbox
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &sb); !apierrors.IsNotFound(err) {
				t.Errorf("microVM created without its config: %v", err)
			}
			cond := readyCondition(getStandIn(t, c))
			if cond == nil || cond.Reason != "MicroVMConfigError" || !strings.Contains(cond.Message, "not found") {
				t.Errorf("condition %+v", cond)
			}
			select {
			case ev := <-rec.Events:
				if !strings.Contains(ev, "MicroVMConfigError") {
					t.Errorf("event %q", ev)
				}
			default:
				t.Error("no event")
			}
		})
	}

	// Once it exists, the microVM starts.
	objs := tenantConfig()
	r, c, _ := setup(t, append(append(objs[:1:1], objs[2:]...), configStandIn(t))...)
	reconcileOnce(t, r, "tenant", "sb-1")
	if err := c.Create(context.Background(), objs[1]); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	getSandbox(t, c)
}

func TestTwoVolumesOnOnePathIsAConfigError(t *testing.T) {
	p := configStandIn(t)
	wl, _ := podmode.ParseWorkload(p)
	wl.Files = append(wl.Files, wl.Files[0])
	cfg, err := ResolveConfig(context.Background(), fakeReader(t, tenantConfig()...), p, wl)
	if _, ok := isConfigError(err); !ok || !strings.Contains(err.Error(), "used twice") {
		t.Errorf("cfg %v err %v", cfg, err)
	}
}

func TestConfigSecretRemovedWithTheStandIn(t *testing.T) {
	r, c, _ := setup(t, append(tenantConfig(), configStandIn(t))...)
	reconcileOnce(t, r, "tenant", "sb-1")
	if err := c.Delete(context.Background(), getStandIn(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	reconcileOnce(t, r, "tenant", "sb-1")
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &s); !apierrors.IsNotFound(err) {
		t.Errorf("copy Secret kept: %v", err)
	}
}

func TestNoConfigNoSecret(t *testing.T) {
	r, c, _ := setup(t, standIn(t))
	reconcileOnce(t, r, "tenant", "sb-1")
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &s); !apierrors.IsNotFound(err) {
		t.Errorf("a copy Secret for a workload that references none: %v", err)
	}
	if sb := getSandbox(t, c); len(sb.Spec.SecretFiles) != 0 {
		t.Errorf("secretFiles %+v", sb.Spec.SecretFiles)
	}
}

func TestFilePath(t *testing.T) {
	for _, tc := range []struct {
		mount, sub, rel, want string
		ok                    bool
	}{
		{"/etc/x", "", "a/b", "/etc/x/a/b", true},
		{"/etc/x.conf", "x.conf", "x.conf", "/etc/x.conf", true},
		{"/etc/d", "dir", "dir/k", "/etc/d/k", true},
		{"/etc/d", "dir", "other", "", false},
		{"/etc/d", "dir", "dirty", "", false},
	} {
		got, ok := filePath(podmode.FileMount{Path: tc.mount, SubPath: tc.sub}, tc.rel)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%+v: %q %v", tc, got, ok)
		}
	}
}
