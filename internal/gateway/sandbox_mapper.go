package gateway

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

// sandboxNetworkMode returns spec.network.mode, defaulting the empty pre-
// admission value to "restricted" (the CRD default) so the UI row is accurate.
func sandboxNetworkMode(mode sandboxv1alpha1.SandboxNetworkMode) string {
	if mode == "" {
		return string(sandboxv1alpha1.SandboxNetworkRestricted)
	}
	return string(mode)
}

// sandboxToProto maps a SwiftSandbox to the flat, UI-shaped inventory row,
// stamped with its member cluster (the D2 dimension). It reads only the sandbox
// object — no cross-resource lookups — so a List stays one round-trip per
// cluster; the expanded config is GetSandboxDetail.
func sandboxToProto(cluster string, s *sandboxv1alpha1.SwiftSandbox) *kubeswiftv1.Sandbox {
	out := &kubeswiftv1.Sandbox{
		Ref:         &kubeswiftv1.ObjectRef{Cluster: cluster, Namespace: s.Namespace, Name: s.Name},
		Phase:       string(s.Status.Phase),
		Image:       s.Spec.Image,
		NodeName:    s.Status.NodeName,
		NetworkMode: sandboxNetworkMode(s.Spec.Network.Mode),
		Cpu:         s.Spec.CPU,
		MemoryMib:   s.Spec.Memory.Value() >> 20,
	}
	if s.Spec.PoolRef != nil {
		out.PoolRef = s.Spec.PoolRef.Name
	}
	if s.Status.Network != nil {
		out.PrimaryIp = s.Status.Network.PrimaryIP
	}
	if s.Status.ExitCode != nil {
		out.ExitCode = *s.Status.ExitCode
	}
	if !s.CreationTimestamp.IsZero() {
		out.CreatedAt = timestamppb.New(s.CreationTimestamp.Time)
	}
	if s.Status.TerminalAt != nil {
		out.TerminalAt = timestamppb.New(s.Status.TerminalAt.Time)
	}
	for i := range s.Status.Conditions {
		out.Conditions = append(out.Conditions, conditionToProto(&s.Status.Conditions[i]))
	}
	if len(s.Labels) > 0 {
		out.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

// sandboxSpecToProto surfaces the structured config for the drawer + the Clone
// pre-fill (mirrors guestSpecToProto).
func sandboxSpecToProto(s *sandboxv1alpha1.SwiftSandbox) *kubeswiftv1.SandboxSpec {
	spec := &kubeswiftv1.SandboxSpec{
		Image:       s.Spec.Image,
		Command:     s.Spec.Command,
		Args:        s.Spec.Args,
		WorkingDir:  s.Spec.WorkingDir,
		NetworkMode: sandboxNetworkMode(s.Spec.Network.Mode),
	}
	if s.Spec.PoolRef != nil {
		spec.PoolRef = s.Spec.PoolRef.Name
	}
	if s.Spec.KernelProfileRef != nil {
		spec.KernelProfileRef = s.Spec.KernelProfileRef.Name
	}
	if s.Spec.Timeout != nil {
		spec.Timeout = s.Spec.Timeout.Duration.String()
	}
	if s.Spec.TTL != nil {
		spec.Ttl = s.Spec.TTL.Duration.String()
	}
	if len(s.Spec.Env) > 0 {
		spec.Env = make(map[string]string, len(s.Spec.Env))
		for _, e := range s.Spec.Env {
			spec.Env[e.Name] = e.Value
		}
	}
	return spec
}

// sandboxPoolToProto maps a SwiftSandboxPool to the flat, UI-shaped row (the
// warm buffer + its scale numbers).
func sandboxPoolToProto(cluster string, p *sandboxv1alpha1.SwiftSandboxPool) *kubeswiftv1.SandboxPool {
	out := &kubeswiftv1.SandboxPool{
		Ref:             &kubeswiftv1.ObjectRef{Cluster: cluster, Namespace: p.Namespace, Name: p.Name},
		Phase:           string(p.Status.Phase),
		Image:           p.Spec.Image,
		MinWarm:         p.Spec.MinWarm,
		MaxWarm:         p.Spec.MaxWarm,
		WarmReplicas:    p.Status.WarmReplicas,
		ClaimedReplicas: p.Status.ClaimedReplicas,
		Cpu:             p.Spec.CPU,
		MemoryMib:       p.Spec.Memory.Value() >> 20,
		NetworkMode:     sandboxNetworkMode(p.Spec.Network.Mode),
	}
	if !p.CreationTimestamp.IsZero() {
		out.CreatedAt = timestamppb.New(p.CreationTimestamp.Time)
	}
	for i := range p.Status.Conditions {
		out.Conditions = append(out.Conditions, conditionToProto(&p.Status.Conditions[i]))
	}
	return out
}
