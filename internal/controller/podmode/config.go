package podmode

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// A stand-in's Secrets and ConfigMaps are in its own namespace, and a
// SwiftSandbox reads Secrets only in its own (the pod-mode namespace). So the
// controller copies exactly the keys the workload references, when it creates
// each microVM, into one Secret in the pod-mode namespace named like the
// SwiftSandbox. The SwiftSandbox refers to that Secret by key (env
// secretKeyRef, secretFiles), so no value lands in the SwiftSandbox. It reads
// only from the stand-in's namespace and only what the Pod admitted there
// names, which whoever created the Pod could mount anyway.

const (
	// maxConfigBytes keeps the copy under the 1 MiB Secret limit.
	maxConfigBytes = 1<<20 - 16<<10
	// fileItemsPerEntry and maxFileEntries are SwiftSandbox secretFiles
	// limits.
	fileItemsPerEntry = 32
	maxFileEntries    = 16
	// defaultFileMode is the Kubernetes default for Secret and ConfigMap
	// volumes.
	defaultFileMode int32 = 0o644
)

// ConfigError is a reference the workload cannot be started with yet (a
// missing Secret, ConfigMap or key) or at all (too large). The microVM is
// not created; the stand-in says why.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return e.Msg }

func configErrorf(f string, args ...any) error { return &ConfigError{Msg: fmt.Sprintf(f, args...)} }

// Config is what a microVM gets from the stand-in's Secrets and ConfigMaps.
type Config struct {
	// Data is the copy Secret's data; empty when nothing is referenced.
	Data  map[string][]byte
	Env   []corev1.EnvVar
	Files []sandboxv1alpha1.SandboxSecretFile
	// Skipped are envFrom keys left out because they cannot name a variable
	// in the microVM (podmode.ShellName), as "<Kind> <name>: <key>".
	Skipped []string
}

// ConfigSecretName is the copy Secret of a stand-in, in the pod-mode
// namespace.
func ConfigSecretName(pod *corev1.Pod) string { return SandboxName(pod) }

type configSource struct {
	data  map[string][]byte
	found bool
}

type configResolver struct {
	reader    client.Reader
	namespace string
	secret    string
	cache     map[string]configSource
	data      map[string][]byte
	// keys names each copied source key in the copy Secret: k0, k1, ... in
	// the order they are referenced, valid and short whatever the source
	// names are. The copy is rewritten whole for each microVM.
	keys map[string]string
}

func (c *configResolver) load(ctx context.Context, kind, name string) (configSource, error) {
	id := kind + "/" + name
	if s, ok := c.cache[id]; ok {
		return s, nil
	}
	key := types.NamespacedName{Namespace: c.namespace, Name: name}
	var s configSource
	switch kind {
	case "secret":
		var o corev1.Secret
		err := c.reader.Get(ctx, key, &o)
		if err != nil && !apierrors.IsNotFound(err) {
			return s, err
		}
		if err == nil {
			s = configSource{data: o.Data, found: true}
			if s.data == nil {
				s.data = map[string][]byte{}
			}
		}
	case "configmap":
		var o corev1.ConfigMap
		err := c.reader.Get(ctx, key, &o)
		if err != nil && !apierrors.IsNotFound(err) {
			return s, err
		}
		if err == nil {
			s = configSource{data: map[string][]byte{}, found: true}
			for k, v := range o.Data {
				s.data[k] = []byte(v)
			}
			for k, v := range o.BinaryData {
				s.data[k] = v
			}
		}
	}
	c.cache[id] = s
	return s, nil
}

// ref copies one key and returns its key in the copy Secret.
func (c *configResolver) ref(kind, name, key string, val []byte) *corev1.SecretKeySelector {
	id := kind + "/" + name + "/" + key
	k, ok := c.keys[id]
	if !ok {
		k = fmt.Sprintf("k%d", len(c.keys))
		c.keys[id] = k
	}
	c.data[k] = val
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: c.secret}, Key: k}
}

func kindName(kind string) string {
	if kind == "configmap" {
		return "ConfigMap"
	}
	return "Secret"
}

func notFound(kind, ns, name string) error {
	return configErrorf("%s %s/%s not found", kindName(kind), ns, name)
}

func keyNotFound(kind, ns, name, key string) error {
	return configErrorf("key %q not found in %s %s/%s", key, kindName(kind), ns, name)
}

// ResolveConfig reads what the workload references in the stand-in's
// namespace through reader. It returns a *ConfigError for a missing,
// non-optional reference, as the kubelet would not start the container.
func ResolveConfig(ctx context.Context, reader client.Reader, pod *corev1.Pod, wl *podmode.Workload) (*Config, error) {
	c := &configResolver{reader: reader, namespace: pod.Namespace, secret: ConfigSecretName(pod),
		cache: map[string]configSource{}, data: map[string][]byte{}, keys: map[string]string{}}
	ns := pod.Namespace

	// envFrom first, then env; a later variable of the same name wins.
	var env []corev1.EnvVar
	var skipped []string
	at := map[string]int{}
	set := func(e corev1.EnvVar) {
		if i, ok := at[e.Name]; ok {
			env[i] = e
			return
		}
		at[e.Name] = len(env)
		env = append(env, e)
	}
	for _, ef := range wl.EnvFrom {
		kind, name, optional := "secret", "", false
		switch {
		case ef.SecretRef != nil:
			name, optional = ef.SecretRef.Name, ptr.Deref(ef.SecretRef.Optional, false)
		case ef.ConfigMapRef != nil:
			kind, name, optional = "configmap", ef.ConfigMapRef.Name, ptr.Deref(ef.ConfigMapRef.Optional, false)
		default:
			continue
		}
		src, err := c.load(ctx, kind, name)
		if err != nil {
			return nil, err
		}
		if !src.found {
			if optional {
				continue
			}
			return nil, notFound(kind, ns, name)
		}
		for _, k := range sortedKeys(src.data) {
			// Skipped as the kubelet skips keys that are not variable
			// names, but by the guest's stricter rule; reported.
			if !podmode.ShellName(ef.Prefix + k) {
				skipped = append(skipped, fmt.Sprintf("%s %s: %s", kindName(kind), name, k))
				continue
			}
			set(corev1.EnvVar{Name: ef.Prefix + k, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: c.ref(kind, name, k, src.data[k])}})
		}
	}
	for _, e := range wl.Env {
		if !podmode.ShellName(e.Name) {
			return nil, configErrorf("env %q cannot name a variable in the microVM", e.Name)
		}
		vf := e.ValueFrom
		if vf == nil || (vf.SecretKeyRef == nil && vf.ConfigMapKeyRef == nil) {
			set(e)
			continue
		}
		kind, name, key, optional := "secret", "", "", false
		if r := vf.SecretKeyRef; r != nil {
			name, key, optional = r.Name, r.Key, ptr.Deref(r.Optional, false)
		} else {
			r := vf.ConfigMapKeyRef
			kind, name, key, optional = "configmap", r.Name, r.Key, ptr.Deref(r.Optional, false)
		}
		src, err := c.load(ctx, kind, name)
		if err != nil {
			return nil, err
		}
		val, ok := src.data[key]
		switch {
		case ok:
			set(corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: c.ref(kind, name, key, val)}})
		case optional:
		case !src.found:
			return nil, notFound(kind, ns, name)
		default:
			return nil, keyNotFound(kind, ns, name, key)
		}
	}

	var items []sandboxv1alpha1.SandboxSecretFileItem
	for _, f := range wl.Files {
		kind, name, optional := "secret", "", false
		var keyItems []corev1.KeyToPath
		var defMode *int32
		switch {
		case f.Secret != nil:
			name, optional, keyItems, defMode = f.Secret.SecretName, ptr.Deref(f.Secret.Optional, false), f.Secret.Items, f.Secret.DefaultMode
		case f.ConfigMap != nil:
			kind, name, optional, keyItems, defMode = "configmap", f.ConfigMap.Name, ptr.Deref(f.ConfigMap.Optional, false), f.ConfigMap.Items, f.ConfigMap.DefaultMode
		default:
			continue
		}
		src, err := c.load(ctx, kind, name)
		if err != nil {
			return nil, err
		}
		if !src.found {
			if optional {
				continue
			}
			return nil, notFound(kind, ns, name)
		}
		if len(keyItems) == 0 {
			for _, k := range sortedKeys(src.data) {
				keyItems = append(keyItems, corev1.KeyToPath{Key: k, Path: k})
			}
		}
		for _, it := range keyItems {
			val, ok := src.data[it.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, keyNotFound(kind, ns, name, it.Key)
			}
			target, ok := filePath(f, it.Path)
			if !ok {
				continue
			}
			mode := defaultFileMode
			if defMode != nil {
				mode = *defMode
			}
			if it.Mode != nil {
				mode = *it.Mode
			}
			items = append(items, sandboxv1alpha1.SandboxSecretFileItem{Key: c.ref(kind, name, it.Key, val).Key, Path: target, Mode: ptr.To(mode)})
		}
	}
	if n := (len(items) + fileItemsPerEntry - 1) / fileItemsPerEntry; n > maxFileEntries {
		return nil, configErrorf("%d files from Secret and ConfigMap volumes, at most %d", len(items), maxFileEntries*fileItemsPerEntry)
	}
	cfg := &Config{Data: c.data, Env: env, Skipped: skipped}
	for len(items) > 0 {
		n := min(len(items), fileItemsPerEntry)
		cfg.Files = append(cfg.Files, sandboxv1alpha1.SandboxSecretFile{SecretName: c.secret, Items: items[:n]})
		items = items[n:]
	}
	size := 0
	for k, v := range c.data {
		size += len(k) + len(v)
	}
	if size > maxConfigBytes {
		return nil, configErrorf("the referenced Secret and ConfigMap keys hold %d bytes, at most %d", size, maxConfigBytes)
	}
	// What the SwiftSandbox webhook would refuse (two volumes writing one
	// path, say) is a config error too, not a create that fails forever.
	spec := &sandboxv1alpha1.SwiftSandboxSpec{Env: cfg.Env, SecretFiles: cfg.Files}
	if err := sandboxwebhook.ValidateEnv(spec.Env); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	if err := sandboxwebhook.ValidateSecretFiles(spec); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	return cfg, nil
}

// filePath is where the file of a volume key (at rel inside the volume) goes
// in the guest, and false when a subPath mount leaves it out.
func filePath(f podmode.FileMount, rel string) (string, bool) {
	rel = strings.Trim(rel, "/")
	switch {
	case f.SubPath == "":
		return path.Join(f.Path, rel), true
	case rel == f.SubPath:
		return f.Path, true
	case strings.HasPrefix(rel, f.SubPath+"/"):
		return path.Join(f.Path, strings.TrimPrefix(rel, f.SubPath+"/")), true
	}
	return "", false
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureConfigSecret writes the copy Secret, before the microVM is created.
func (r *Reconciler) ensureConfigSecret(ctx context.Context, pod *corev1.Pod, data map[string][]byte) error {
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigSecretName(pod),
			Namespace: r.Namespace,
			Labels: map[string]string{
				podmode.LabelUID:              string(pod.UID),
				podmode.LabelStandInNamespace: pod.Namespace,
			},
			Annotations: map[string]string{podmode.AnnotationStandIn: pod.Namespace + "/" + pod.Name},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	var cur corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(want), &cur)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	if apiequality.Semantic.DeepEqual(cur.Data, want.Data) && cur.Labels[podmode.LabelUID] == string(pod.UID) {
		return nil
	}
	cur.Labels, cur.Annotations, cur.Data = want.Labels, want.Annotations, want.Data
	return r.Update(ctx, &cur)
}

// deleteConfigSecret removes a stand-in's copy Secret, if any.
func (r *Reconciler) deleteConfigSecret(ctx context.Context, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace}}
	return client.IgnoreNotFound(r.Delete(ctx, s))
}

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func isConfigError(err error) (*ConfigError, bool) {
	var ce *ConfigError
	ok := errors.As(err, &ce)
	return ce, ok
}
