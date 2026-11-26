// Package podmode turns a Pod that asks for the kubeswift-microvm RuntimeClass
// into an unprivileged stand-in Pod plus the description of the microVM that
// runs its workload.
//
// The stand-in stays in the tenant namespace, so everything that selects or
// owns Pods (Deployments, agent-sandbox, quota, NetworkPolicy, Services) keeps
// working. It runs one container, the pod-shim, which forwards the workload's
// ports to the launcher of a SwiftSandbox in the pod-mode namespace. The
// workload itself, as recorded in AnnotationWorkload at admission, runs in that
// SwiftSandbox's microVM.
//
// Every Pod field the rewrite cannot carry into the microVM is refused by name.
// Nothing is dropped silently, except fields that only harden the container
// (the microVM is the boundary) and the injected ServiceAccount token, which
// the stand-in does not mount.
package podmode

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

const (
	// RuntimeClassName is the RuntimeClass a Pod names to run in a microVM.
	// Its handler exists on no node, so a Pod that reaches a kubelet without
	// the rewrite never starts.
	RuntimeClassName = "kubeswift-microvm"

	// NamespaceLabel opts a namespace into pod mode (value "enabled").
	NamespaceLabel = "kubeswift.io/pod-mode"
	// NamespaceLabelValue is the value NamespaceLabel must have.
	NamespaceLabelValue = "enabled"

	// LabelStandIn marks a stand-in Pod (value "true").
	LabelStandIn = "podmode.kubeswift.io/stand-in"
	// LabelUID is the stand-in's UID, on the stand-in (set by the controller)
	// and on its SwiftSandbox. NetworkPolicies select the stand-in by it.
	LabelUID = "podmode.kubeswift.io/uid"
	// LabelStandInNamespace is the stand-in's namespace, on its SwiftSandbox.
	LabelStandInNamespace = "podmode.kubeswift.io/stand-in-namespace"
	// AnnotationStandIn is "<namespace>/<name>" of the stand-in, on its
	// SwiftSandbox.
	AnnotationStandIn = "podmode.kubeswift.io/stand-in"
	// AnnotationWorkload is the workload the stand-in replaced (JSON Workload).
	AnnotationWorkload = "podmode.kubeswift.io/workload"
	// AnnotationTarget is the launcher pod IP the shim forwards to, written by
	// the controller and read by the shim through the downward API.
	AnnotationTarget = "podmode.kubeswift.io/target"
	// AnnotationRun numbers the stand-in's microVMs: 1 for the first, one
	// more each time a restart boots a new one. Written by the controller on
	// the stand-in and on each SwiftSandbox.
	AnnotationRun = "podmode.kubeswift.io/run"
	// AnnotationExit is "<run>:<exit code>" once that run's workload ended,
	// written by the controller; the shim exits with the code.
	AnnotationExit = "podmode.kubeswift.io/exit"
	// AnnotationExitRestarts is the shim container's restart count when
	// AnnotationExit was written: a higher count means the kubelet restarted
	// it, and the controller boots the next run.
	AnnotationExitRestarts = "podmode.kubeswift.io/exit-restarts"
	// ConditionMicroVMReady is the stand-in's readiness gate: True while its
	// microVM runs and the shim forwards to it.
	ConditionMicroVMReady corev1.PodConditionType = "podmode.kubeswift.io/microvm-ready"

	// Finalizer holds the stand-in until its SwiftSandbox is gone.
	Finalizer = "podmode.kubeswift.io/microvm"

	// AnnotationRelay is where the shim reaches the exec and logs relay, as
	// JSON (Relay), written by the controller.
	AnnotationRelay = "podmode.kubeswift.io/relay"

	// ShimVolume is the projected volume the shim reads its annotations and
	// relay token from.
	ShimVolume = "kubeswift-podmode"
	// ShimDir is where ShimVolume is mounted.
	ShimDir = "/etc/podmode"
	// ShimAnnotationsFile is the downward API file holding the annotations.
	ShimAnnotationsFile = ShimDir + "/annotations"
	// ShimTokenFile is the stand-in's ServiceAccount token for the relay.
	ShimTokenFile = ShimDir + "/token"
	// TokenAudience is the only audience of the shim's token: it is good for
	// the relay and nothing else.
	TokenAudience = "kubeswift-podmode"
	// TokenExpirationSeconds is the token's lifetime; the kubelet rotates it.
	TokenExpirationSeconds int64 = 3600

	// RelayExecPath and RelayLogsPath are the relay's endpoints on the
	// controller-manager's webhook server.
	RelayExecPath = "/podmode/exec"
	RelayLogsPath = "/podmode/logs"
	// ExecRequestHeader carries the base64url JSON ExecRequest, so the relay
	// can refuse before the WebSocket upgrade with a readable error.
	ExecRequestHeader = "Kubeswift-Exec-Request"
	// ShimUID is the uid the shim runs as.
	ShimUID int64 = 65532

	// WorkloadVersion is the version of the Workload annotation.
	WorkloadVersion = 1

	// ShimImageEnv overrides the stand-in's image (chart:
	// sandboxMaterialize.podMode.image).
	ShimImageEnv = "KUBESWIFT_POD_SHIM_IMAGE"
	// ShimImageDefault is the fallback when the env is unset.
	ShimImageDefault = "ghcr.io/kubeswift-io/kubeswift/pod-shim:latest"

	defaultMemory = "512Mi"
	minMemory     = "256Mi"
)

// Workload is what a stand-in replaced: the fields of the Pod's one container
// that the microVM runs. Recorded in AnnotationWorkload at admission.
type Workload struct {
	Version int             `json:"v"`
	Image   string          `json:"image"`
	Command []string        `json:"command,omitempty"`
	Args    []string        `json:"args,omitempty"`
	Env     []corev1.EnvVar `json:"env,omitempty"`
	// EnvFrom are the container's envFrom sources (Secrets and ConfigMaps
	// in the stand-in's namespace), resolved when each microVM is created.
	EnvFrom      []corev1.EnvFromSource `json:"envFrom,omitempty"`
	Files        []FileMount            `json:"files,omitempty"`
	WorkingDir   string                 `json:"workingDir,omitempty"`
	Ports        []Port                 `json:"ports,omitempty"`
	RunAsUser    *int64                 `json:"runAsUser,omitempty"`
	RunAsGroup   *int64                 `json:"runAsGroup,omitempty"`
	RunAsNonRoot *bool                  `json:"runAsNonRoot,omitempty"`
	CPU          int32                  `json:"cpu"`
	Memory       string                 `json:"memory"`
}

// Relay is how a shim reaches the relay: Addr is dialled, the certificate is
// checked against CA for ServerName. Addr is an IP so the stand-in's DNS
// settings (often dnsPolicy None) do not matter.
type Relay struct {
	Addr       string `json:"addr"`
	ServerName string `json:"serverName"`
	CA         string `json:"ca"`
}

// ExecRequest is the first message a shim sends on the exec relay. The relay
// builds the guest agent's request from it, so a shim can only exec.
type ExecRequest struct {
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// FileMount is a Secret or ConfigMap volume the workload container mounts.
// Its keys are written into the guest as files under Path (or, with SubPath,
// the one file or directory SubPath names), before the workload starts.
type FileMount struct {
	Path      string                        `json:"path"`
	SubPath   string                        `json:"subPath,omitempty"`
	Secret    *corev1.SecretVolumeSource    `json:"secret,omitempty"`
	ConfigMap *corev1.ConfigMapVolumeSource `json:"configMap,omitempty"`
}

// Port is one TCP port of the workload.
type Port struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

// ShimImage is the stand-in's image: the env override, else the default.
func ShimImage() string {
	if v := os.Getenv(ShimImageEnv); v != "" {
		return v
	}
	return ShimImageDefault
}

// ParseWorkload reads the Workload annotation of a stand-in.
func ParseWorkload(pod *corev1.Pod) (*Workload, error) {
	raw, ok := pod.Annotations[AnnotationWorkload]
	if !ok {
		return nil, fmt.Errorf("annotation %s is missing", AnnotationWorkload)
	}
	var w Workload
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return nil, fmt.Errorf("annotation %s: %w", AnnotationWorkload, err)
	}
	if w.Version != WorkloadVersion {
		return nil, fmt.Errorf("annotation %s: version %d, want %d", AnnotationWorkload, w.Version, WorkloadVersion)
	}
	if w.Image == "" {
		return nil, fmt.Errorf("annotation %s: no image", AnnotationWorkload)
	}
	return &w, nil
}

// Wants is whether the Pod asks for the microVM RuntimeClass.
func Wants(pod *corev1.Pod) bool {
	return pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == RuntimeClassName
}

// IsStandIn is whether the Pod carries the stand-in label.
func IsStandIn(pod *corev1.Pod) bool {
	return pod.Labels[LabelStandIn] == "true"
}

// RefusedError lists the fields a rewrite cannot carry into the microVM.
type RefusedError struct{ Fields []string }

func (e *RefusedError) Error() string {
	return "not supported in pod mode: " + strings.Join(e.Fields, ", ")
}

// Rewrite returns the stand-in for a Pod that wants the microVM RuntimeClass,
// with the workload recorded in AnnotationWorkload. shimImage is the image the
// stand-in's container runs (sandbox-materialize). It returns a *RefusedError
// naming every field it cannot carry.
func Rewrite(pod *corev1.Pod, shimImage string) (*corev1.Pod, error) {
	var refused []string
	refuse := func(f string, args ...any) { refused = append(refused, fmt.Sprintf(f, args...)) }

	s := &pod.Spec
	if len(s.Containers) != 1 {
		refuse("spec.containers (exactly one container)")
		return nil, &RefusedError{Fields: refused}
	}
	tokenVolumes := map[string]bool{}
	emptyDirs := map[string]bool{}
	configVolumes := map[string]corev1.Volume{}
	for _, v := range s.Volumes {
		switch {
		case v.EmptyDir != nil:
			emptyDirs[v.Name] = true
		case isInjectedToken(v):
			tokenVolumes[v.Name] = true
		case v.Secret != nil, v.ConfigMap != nil:
			configVolumes[v.Name] = v
		default:
			refuse("spec.volumes[%s] (only emptyDir, secret and configMap; the guest root filesystem is writable)", v.Name)
		}
	}
	for _, k := range unmappedPodSpec(s) {
		refuse("spec.%s", k)
	}
	if sc := s.SecurityContext; sc != nil {
		if len(sc.Sysctls) > 0 {
			refuse("spec.securityContext.sysctls")
		}
		if sc.WindowsOptions != nil {
			refuse("spec.securityContext.windowsOptions")
		}
	}

	c := &s.Containers[0]
	p := fmt.Sprintf("spec.containers[%s]", c.Name)
	for _, k := range unmappedContainer(c) {
		refuse("%s.%s", p, k)
	}
	for _, e := range c.Env {
		if vf := e.ValueFrom; vf != nil && vf.SecretKeyRef == nil && vf.ConfigMapKeyRef == nil {
			refuse("%s.env[%s].valueFrom (only secretKeyRef and configMapKeyRef)", p, e.Name)
		}
	}
	for i, ef := range c.EnvFrom {
		if ef.SecretRef == nil && ef.ConfigMapRef == nil {
			refuse("%s.envFrom[%d] (only secretRef and configMapRef)", p, i)
		}
	}
	var files []FileMount
	for _, m := range c.VolumeMounts {
		v, isConfig := configVolumes[m.Name]
		switch {
		case isConfig:
			mp := m.MountPath
			if !strings.HasPrefix(mp, "/") {
				mp = "/" + mp
			}
			switch {
			case m.SubPathExpr != "":
				refuse("%s.volumeMounts[%s].subPathExpr", p, m.Name)
			case underGuestDir(mp):
				refuse("%s.volumeMounts[%s].mountPath (%s is the guest's own)", p, m.Name, m.MountPath)
			default:
				files = append(files, FileMount{Path: path.Clean(mp), SubPath: strings.Trim(m.SubPath, "/"),
					Secret: v.Secret, ConfigMap: v.ConfigMap})
			}
		case !emptyDirs[m.Name] && !tokenVolumes[m.Name]:
			refuse("%s.volumeMounts[%s]", p, m.Name)
		}
	}
	if len(c.Resources.Claims) > 0 {
		refuse("%s.resources.claims", p)
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.Privileged != nil && *sc.Privileged {
			refuse("%s.securityContext.privileged", p)
		}
		if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
			refuse("%s.securityContext.capabilities.add", p)
		}
		if sc.WindowsOptions != nil {
			refuse("%s.securityContext.windowsOptions", p)
		}
	}
	w := Workload{
		Version:    WorkloadVersion,
		Image:      c.Image,
		Command:    c.Command,
		Args:       c.Args,
		Env:        c.Env,
		EnvFrom:    c.EnvFrom,
		Files:      files,
		WorkingDir: c.WorkingDir,
	}
	seen := map[int32]bool{}
	var shimPorts []corev1.ContainerPort
	for _, cp := range c.Ports {
		switch {
		case cp.Protocol != "" && cp.Protocol != corev1.ProtocolTCP:
			refuse("%s.ports[%d].protocol (TCP only)", p, cp.ContainerPort)
		case cp.HostPort != 0 || cp.HostIP != "":
			refuse("%s.ports[%d].hostPort", p, cp.ContainerPort)
		case seen[cp.ContainerPort]:
			refuse("%s.ports[%d] (declared twice)", p, cp.ContainerPort)
		default:
			seen[cp.ContainerPort] = true
			name := cp.Name
			if name == "" {
				name = "p" + strconv.Itoa(int(cp.ContainerPort))
			}
			w.Ports = append(w.Ports, Port{Name: name, Port: cp.ContainerPort})
			shimPorts = append(shimPorts, corev1.ContainerPort{Name: cp.Name, ContainerPort: cp.ContainerPort, Protocol: corev1.ProtocolTCP})
		}
	}
	if len(refused) > 0 {
		return nil, &RefusedError{Fields: refused}
	}
	if sc := s.SecurityContext; sc != nil {
		w.RunAsUser, w.RunAsGroup, w.RunAsNonRoot = sc.RunAsUser, sc.RunAsGroup, sc.RunAsNonRoot
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			w.RunAsUser = sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			w.RunAsGroup = sc.RunAsGroup
		}
		if sc.RunAsNonRoot != nil {
			w.RunAsNonRoot = sc.RunAsNonRoot
		}
	}
	w.CPU = guestCPU(c.Resources)
	w.Memory = guestMemory(c.Resources)
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}

	out := pod.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Labels[LabelStandIn] = "true"
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[AnnotationWorkload] = string(raw)
	delete(out.Annotations, AnnotationTarget)

	st := &out.Spec
	st.RuntimeClassName = nil
	st.Overhead = nil
	st.AutomountServiceAccountToken = ptr.To(false)
	st.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if st.NodeSelector == nil {
		st.NodeSelector = map[string]string{}
	}
	// The microVM runs on the stand-in's node, so the stand-in must land on a
	// node that runs KubeSwift kernels.
	st.NodeSelector["kubeswift.io/kernel-node"] = "true"
	// Ready only while the microVM runs, whatever probe the Pod has.
	if !hasReadinessGate(st.ReadinessGates) {
		st.ReadinessGates = append(st.ReadinessGates, corev1.PodReadinessGate{ConditionType: ConditionMicroVMReady})
	}
	st.Volumes = []corev1.Volume{ShimVolumeSource()}
	st.Containers = []corev1.Container{StandInContainer(c, shimImage, w.Ports, shimPorts)}
	return out, nil
}

// underGuestDir is whether p is in a directory mounted over the guest root
// after files are written (/proc, /sys, /dev), where a file would be hidden.
func underGuestDir(p string) bool {
	p = path.Clean(p)
	for _, d := range []string{"/proc", "/sys", "/dev"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

func hasReadinessGate(gates []corev1.PodReadinessGate) bool {
	for _, g := range gates {
		if g.ConditionType == ConditionMicroVMReady {
			return true
		}
	}
	return false
}

// ShimVolumeSource is the stand-in's one volume: its annotations (target and
// relay) and a ServiceAccount token whose only audience is the relay.
func ShimVolumeSource() corev1.Volume {
	return corev1.Volume{
		Name: ShimVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{DownwardAPI: &corev1.DownwardAPIProjection{
					Items: []corev1.DownwardAPIVolumeFile{{
						Path:     "annotations",
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations"},
					}},
				}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience:          TokenAudience,
					ExpirationSeconds: ptr.To(TokenExpirationSeconds),
					Path:              "token",
				}},
			},
		}},
	}
}

// shimVolumeOK is whether v is the shim volume, with no token for any other
// audience.
func shimVolumeOK(v corev1.Volume) bool {
	if v.Name != ShimVolume || v.Projected == nil {
		return false
	}
	for _, src := range v.Projected.Sources {
		switch {
		case src.DownwardAPI != nil && src.ServiceAccountToken == nil && src.Secret == nil && src.ConfigMap == nil && src.ClusterTrustBundle == nil && src.PodCertificate == nil:
		case src.ServiceAccountToken != nil && src.ServiceAccountToken.Audience == TokenAudience && src.DownwardAPI == nil && src.Secret == nil && src.ConfigMap == nil && src.ClusterTrustBundle == nil && src.PodCertificate == nil:
		default:
			return false
		}
	}
	return true
}

// StandInContainer is the stand-in's one container: the shim, under the
// workload container's name, with its ports, readiness probe and resources
// (so quota and scheduling see what the microVM costs).
func StandInContainer(c *corev1.Container, shimImage string, ports []Port, shimPorts []corev1.ContainerPort) corev1.Container {
	list := make([]string, 0, len(ports))
	for _, p := range ports {
		list = append(list, strconv.Itoa(int(p.Port)))
	}
	args := []string{"pod-shim", "--annotations-file", ShimAnnotationsFile}
	if len(list) > 0 {
		args = append(args, "--ports", strings.Join(list, ","))
	}
	return corev1.Container{
		Name:                     c.Name,
		Image:                    shimImage,
		Args:                     args,
		Ports:                    shimPorts,
		ReadinessProbe:           c.ReadinessProbe,
		Resources:                corev1.ResourceRequirements{Requests: c.Resources.Requests, Limits: c.Resources.Limits},
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(ShimUID),
			RunAsGroup:               ptr.To(ShimUID),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Privileged:               ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: ShimVolume, MountPath: ShimDir, ReadOnly: true}},
	}
}

// ShapeOK is whether a stand-in still has exactly the shim: one container
// running shimImage's repository (a stand-in created before an upgrade keeps
// the older tag), no init or ephemeral containers, and no extra volumes. The
// admission policy enforces this; the controller tears down a stand-in that
// fails it anyway (the policy was missing or bypassed).
func ShapeOK(pod *corev1.Pod, shimImage string) bool {
	s := &pod.Spec
	if len(s.Containers) != 1 || len(s.InitContainers) > 0 || len(s.EphemeralContainers) > 0 {
		return false
	}
	c := &s.Containers[0]
	if imageRepository(c.Image) != imageRepository(shimImage) || len(c.Command) > 0 || len(c.Args) == 0 || c.Args[0] != "pod-shim" {
		return false
	}
	if sc := c.SecurityContext; sc == nil || sc.Privileged != nil && *sc.Privileged {
		return false
	}
	for _, v := range s.Volumes {
		if !shimVolumeOK(v) && !isInjectedToken(v) {
			return false
		}
	}
	return true
}

// imageRepository is an image reference without its tag or digest.
func imageRepository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// isInjectedToken is the projected ServiceAccount token volume the
// ServiceAccount admission plugin adds before mutating webhooks run.
func isInjectedToken(v corev1.Volume) bool {
	if !strings.HasPrefix(v.Name, "kube-api-access-") || v.Projected == nil {
		return false
	}
	for _, src := range v.Projected.Sources {
		if src.ServiceAccountToken != nil {
			return true
		}
	}
	return false
}

// unmappedPodSpec names the PodSpec fields set on the Pod that are neither
// carried into the microVM nor meaningful on the stand-in.
func unmappedPodSpec(s *corev1.PodSpec) []string {
	r := s.DeepCopy()
	// Handled by the rewrite.
	r.Containers, r.Volumes, r.RuntimeClassName, r.Overhead = nil, nil, nil, nil
	r.SecurityContext, r.AutomountServiceAccountToken = nil, nil
	// Kept on the stand-in: scheduling, identity and lifecycle of the Pod.
	r.NodeSelector, r.Affinity, r.Tolerations, r.NodeName = nil, nil, nil, ""
	r.SchedulerName, r.PriorityClassName, r.Priority, r.PreemptionPolicy = "", "", nil, nil
	r.TopologySpreadConstraints, r.SchedulingGates, r.ReadinessGates = nil, nil, nil
	r.TerminationGracePeriodSeconds, r.ActiveDeadlineSeconds, r.RestartPolicy = nil, nil, ""
	r.ServiceAccountName, r.DeprecatedServiceAccount, r.ImagePullSecrets = "", "", nil
	r.Hostname, r.Subdomain, r.SetHostnameAsFQDN = "", "", nil
	// The stand-in's own resolver. agent-sandbox sets dnsPolicy None with
	// public resolvers by default; the guest resolves through its launcher.
	r.DNSPolicy, r.DNSConfig, r.EnableServiceLinks = "", nil, nil
	// Hardening of the Pod's own sandbox: the microVM is the boundary.
	r.HostUsers = nil
	if r.OS != nil && r.OS.Name == corev1.Linux {
		r.OS = nil
	}
	return setFields(r)
}

// unmappedContainer names the Container fields set that the rewrite does not
// carry. Probes other than readiness, lifecycle hooks, stdin/tty and envFrom
// are refused rather than ignored.
func unmappedContainer(c *corev1.Container) []string {
	r := c.DeepCopy()
	r.Name, r.Image, r.Command, r.Args, r.WorkingDir = "", "", nil, nil, ""
	r.Env, r.EnvFrom, r.Ports, r.VolumeMounts, r.SecurityContext = nil, nil, nil, nil, nil
	r.Resources, r.ResizePolicy = corev1.ResourceRequirements{}, nil
	r.ReadinessProbe = nil
	r.ImagePullPolicy, r.TerminationMessagePath, r.TerminationMessagePolicy = "", "", ""
	return setFields(r)
}

// setFields is the JSON names of the fields of v that are set.
func setFields(v any) []string {
	raw, _ := json.Marshal(v)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	var out []string
	for k, val := range m {
		switch string(val) {
		case "null", "false", "[]", `""`, "0":
			continue
		case "{}":
			// Container.resources is a struct, so it marshals even when
			// zeroed; any other empty object was set by the client.
			if k == "resources" {
				continue
			}
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guestCPU is the microVM's vCPU count: the container's CPU limit, else its
// request, rounded up, at least 1.
func guestCPU(r corev1.ResourceRequirements) int32 {
	q, ok := r.Limits[corev1.ResourceCPU]
	if !ok {
		q, ok = r.Requests[corev1.ResourceCPU]
	}
	if !ok {
		return 1
	}
	return int32(math.Max(1, math.Ceil(q.AsApproximateFloat64())))
}

// guestMemory is the microVM's memory: the container's memory limit, else its
// request, else 512Mi, at least 256Mi.
func guestMemory(r corev1.ResourceRequirements) string {
	q, ok := r.Limits[corev1.ResourceMemory]
	if !ok {
		q, ok = r.Requests[corev1.ResourceMemory]
	}
	if !ok {
		return defaultMemory
	}
	if q.Cmp(resource.MustParse(minMemory)) < 0 {
		return minMemory
	}
	// Whole MiB: the guest is sized in MiB.
	mi := (q.Value() + (1<<20 - 1)) >> 20
	return strconv.FormatInt(mi, 10) + "Mi"
}

// FormatExit is the AnnotationExit value of run's exit with code.
func FormatExit(run, code int) string { return strconv.Itoa(run) + ":" + strconv.Itoa(code) }

// ParseExit reads an AnnotationExit value.
func ParseExit(v string) (run, code int, ok bool) {
	r, c, found := strings.Cut(v, ":")
	if !found {
		return 0, 0, false
	}
	run, err1 := strconv.Atoi(r)
	code, err2 := strconv.Atoi(c)
	return run, code, err1 == nil && err2 == nil && run > 0
}

// Restarts is whether the kubelet restarts a container that exited with
// code under policy, as it does for any Pod.
func Restarts(policy corev1.RestartPolicy, code int) bool {
	switch policy {
	case corev1.RestartPolicyNever:
		return false
	case corev1.RestartPolicyOnFailure:
		return code != 0
	default:
		return true
	}
}
