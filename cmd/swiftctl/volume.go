package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// swiftctl volume: thin wrappers over SwiftVolumeAttachment (volume hotplug,
// docs/storage/volume-hotplug.md). Everything they do is a plain create,
// delete or list of that object.

var volumeCmd = &cobra.Command{
	Use:   "volume",
	Short: "Hot-plug Block PVCs into running guests (SwiftVolumeAttachment)",
	Long: `Attach, detach and list hot-plugged volumes. Each command creates, deletes or
lists SwiftVolumeAttachment objects; volume hotplug must be enabled in the chart
(swiftGuest.volumeHotplug.enabled).`,
}

var (
	volumeName     string
	volumeReadOnly bool
	volumeForce    bool
	volumeWait     bool
	volumeTimeout  time.Duration
)

var volumeAttachCmd = &cobra.Command{
	Use:   "attach GUEST PVC",
	Short: "Hot-plug a Block PVC into a running guest",
	Long: `Create a SwiftVolumeAttachment for PVC on GUEST. The guest sees the disk as
/dev/disk/by-id/virtio-<serial>; "swiftctl volume list" shows the serial.`,
	Example: `  swiftctl volume attach db data-1 --wait
  swiftctl -n team-a volume attach db scratch --read-only --name db-scratch`,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE:         runVolumeAttach,
}

var volumeDetachCmd = &cobra.Command{
	Use:   "detach GUEST PVC|ATTACHMENT",
	Short: "Detach a hot-plugged volume",
	Long: `Delete the SwiftVolumeAttachment of PVC (or the attachment of that name) on
GUEST. The guest is asked to let go of the disk first; while it still uses it
the detach waits (DetachBlocked). --force detaches without asking the guest,
which can lose data the guest has not written.`,
	Example: `  swiftctl volume detach db data-1 --wait
  swiftctl volume detach db data-1 --force`,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE:         runVolumeDetach,
}

var volumeListCmd = &cobra.Command{
	Use:          "list [GUEST]",
	Short:        "List hot-plugged volumes, of one guest or the namespace",
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runVolumeList,
}

func init() {
	volumeAttachCmd.Flags().StringVar(&volumeName, "name", "", "attachment name (default <guest>-<pvc>)")
	volumeAttachCmd.Flags().BoolVar(&volumeReadOnly, "read-only", false, "attach the disk read-only")
	volumeAttachCmd.Flags().BoolVar(&volumeWait, "wait", false, "wait until the guest sees the disk (Attached) or it fails")
	volumeAttachCmd.Flags().DurationVar(&volumeTimeout, "timeout", 5*time.Minute, "how long --wait waits")
	volumeDetachCmd.Flags().BoolVar(&volumeForce, "force", false, "detach without asking the guest (kubeswift.io/force-detach)")
	volumeDetachCmd.Flags().BoolVar(&volumeWait, "wait", false, "wait until the volume is released")
	volumeDetachCmd.Flags().DurationVar(&volumeTimeout, "timeout", 5*time.Minute, "how long --wait waits")
	volumeCmd.AddCommand(volumeAttachCmd, volumeDetachCmd, volumeListCmd)
}

func volumeClient() (client.Client, error) {
	config, err := kubeConfig.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return client.New(config, client.Options{Scheme: scheme.Scheme})
}

// attachmentName is the default attachment name for guest and pvc.
func attachmentName(guest, pvc string) (string, error) {
	name := guest + "-" + pvc
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", fmt.Errorf("default attachment name %q is not valid (%s); pass --name", name, errs[0])
	}
	return name, nil
}

func runVolumeAttach(cmd *cobra.Command, args []string) error {
	guest, pvc := args[0], args[1]
	ns := getNamespace()
	c, err := volumeClient()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	name := volumeName
	if name == "" {
		if name, err = attachmentName(guest, pvc); err != nil {
			return err
		}
	}
	sva := &swiftv1alpha1.SwiftVolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{
			GuestRef: corev1.LocalObjectReference{Name: guest},
			PVCRef:   corev1.LocalObjectReference{Name: pvc},
			ReadOnly: volumeReadOnly,
		},
	}
	if err := c.Create(ctx, sva); err != nil {
		return fmt.Errorf("create SwiftVolumeAttachment %s/%s: %w", ns, name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "swiftvolumeattachment/%s created\n", name)
	if !volumeWait {
		return nil
	}
	return waitAttachment(ctx, c, cmd.OutOrStdout(), ns, name, volumeTimeout)
}

// waitAttachment waits until attachment name is Attached (prints its serial)
// or Failed (returns its reason).
func waitAttachment(ctx context.Context, c client.Client, out io.Writer, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var sva swiftv1alpha1.SwiftVolumeAttachment
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sva); err != nil {
			return err
		}
		switch sva.Status.Phase {
		case swiftv1alpha1.VolumeAttachmentAttached:
			fmt.Fprintf(out, "attached: the guest sees /dev/disk/by-id/virtio-%s\n", sva.Status.Serial)
			return nil
		case swiftv1alpha1.VolumeAttachmentFailed:
			return fmt.Errorf("attachment %s failed: %s", name, readyMessageOf(&sva))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: attachment %s is %s: %s", timeout, name, phaseOf(&sva), readyMessageOf(&sva))
		}
		time.Sleep(2 * time.Second)
	}
}

// findAttachment returns guest's attachment named ref, else the one of PVC ref.
func findAttachment(ctx context.Context, c client.Client, ns, guest, ref string) (*swiftv1alpha1.SwiftVolumeAttachment, error) {
	var sva swiftv1alpha1.SwiftVolumeAttachment
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref}, &sva)
	if err == nil && sva.Spec.GuestRef.Name == guest {
		return &sva, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	var list swiftv1alpha1.SwiftVolumeAttachmentList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var found []*swiftv1alpha1.SwiftVolumeAttachment
	for i := range list.Items {
		a := &list.Items[i]
		if a.Spec.GuestRef.Name == guest && a.Spec.PVCRef.Name == ref {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no attachment of PVC or named %q on SwiftGuest %s/%s", ref, ns, guest)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("PVC %s has %d attachments on SwiftGuest %s/%s; name one", ref, len(found), ns, guest)
}

func runVolumeDetach(cmd *cobra.Command, args []string) error {
	guest, ref := args[0], args[1]
	ns := getNamespace()
	c, err := volumeClient()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sva, err := findAttachment(ctx, c, ns, guest, ref)
	if err != nil {
		return err
	}
	if volumeForce && sva.Annotations[swiftv1alpha1.AnnotationForceDetach] != "true" {
		base := sva.DeepCopy()
		if sva.Annotations == nil {
			sva.Annotations = map[string]string{}
		}
		sva.Annotations[swiftv1alpha1.AnnotationForceDetach] = "true"
		if err := c.Patch(ctx, sva, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("set %s on %s: %w", swiftv1alpha1.AnnotationForceDetach, sva.Name, err)
		}
	}
	if err := c.Delete(ctx, sva); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete SwiftVolumeAttachment %s/%s: %w", ns, sva.Name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "swiftvolumeattachment/%s deleted (detaching)\n", sva.Name)
	if !volumeWait {
		return nil
	}
	deadline := time.Now().Add(volumeTimeout)
	for {
		var cur swiftv1alpha1.SwiftVolumeAttachment
		err := c.Get(ctx, client.ObjectKeyFromObject(sva), &cur)
		if apierrors.IsNotFound(err) {
			fmt.Fprintln(cmd.OutOrStdout(), "detached")
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %s", volumeTimeout, readyMessageOf(&cur))
		}
		time.Sleep(2 * time.Second)
	}
}

func runVolumeList(cmd *cobra.Command, args []string) error {
	ns := getNamespace()
	c, err := volumeClient()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var list swiftv1alpha1.SwiftVolumeAttachmentList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("list SwiftVolumeAttachments: %w", err)
	}
	guest := ""
	if len(args) == 1 {
		guest = args[0]
	}
	return printAttachments(cmd.OutOrStdout(), list.Items, guest, time.Now())
}

func printAttachments(out io.Writer, items []swiftv1alpha1.SwiftVolumeAttachment, guest string, now time.Time) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tGUEST\tPVC\tMODE\tPHASE\tSERIAL\tAGE")
	n := 0
	for i := range items {
		a := &items[i]
		if guest != "" && a.Spec.GuestRef.Name != guest {
			continue
		}
		mode := "rw"
		if a.Spec.ReadOnly {
			mode = "ro"
		}
		serial := a.Status.Serial
		if serial == "" {
			serial = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Spec.GuestRef.Name, a.Spec.PVCRef.Name, mode,
			phaseOf(a), serial, duration.HumanDuration(now.Sub(a.CreationTimestamp.Time)))
		n++
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(out, "no hot-plugged volumes")
	}
	return nil
}

func phaseOf(a *swiftv1alpha1.SwiftVolumeAttachment) string {
	if a.Status.Phase == "" {
		return string(swiftv1alpha1.VolumeAttachmentPending)
	}
	return string(a.Status.Phase)
}

func readyMessageOf(a *swiftv1alpha1.SwiftVolumeAttachment) string {
	for _, c := range a.Status.Conditions {
		if c.Type == swiftv1alpha1.VolumeAttachmentConditionReady {
			return c.Message
		}
	}
	return ""
}
