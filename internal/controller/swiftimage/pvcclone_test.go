package swiftimage

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
)

func pvcCloneImage(rootDisk string) *imagev1alpha1.SwiftImage {
	img := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{PVCClone: &imagev1alpha1.PVCCloneSource{Name: "src"}},
		},
	}
	if rootDisk != "" {
		q := resource.MustParse(rootDisk)
		img.Spec.RootDisk = &imagev1alpha1.SwiftImageRootDiskSpec{Size: &q}
	}
	return img
}

func sourcePVC(mode corev1.PersistentVolumeMode, phase corev1.PersistentVolumeClaimPhase, capacity string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &mode},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    phase,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
		},
	}
}

func podUsing(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "root", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "src"}}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func reconcileImage(t *testing.T, objs ...client.Object) (*imagev1alpha1.SwiftImage, ctrl.Result, client.Client) {
	t.Helper()
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&imagev1alpha1.SwiftImage{}).WithObjects(objs...).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: "img", Namespace: "default"}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got imagev1alpha1.SwiftImage
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if errs := metav1validation.ValidateConditions(got.Status.Conditions, field.NewPath("status", "conditions")); len(errs) > 0 {
		t.Errorf("the apiserver would refuse these conditions: %v", errs.ToAggregate())
	}
	return &got, res, c
}

func sourceReady(img *imagev1alpha1.SwiftImage) *metav1.Condition {
	for i := range img.Status.Conditions {
		if img.Status.Conditions[i].Type == ConditionSourceReady {
			return &img.Status.Conditions[i]
		}
	}
	return nil
}

func importJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	key := types.NamespacedName{Namespace: "default", Name: names.JobName(importJobNamePrefix+"img", "")}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("import job: %v", err)
	}
	return &job
}

// The import waits, Pending and retried, while the source cannot be copied
// consistently, and says why. A pod that has finished does not hold it.
func TestPVCClone_WaitsForItsSource(t *testing.T) {
	block := corev1.PersistentVolumeBlock
	cases := map[string]struct {
		objs   []client.Object
		reason string
		inMsg  string
	}{
		"missing":  {reason: ReasonSourceNotFound, inMsg: `"src" not found`},
		"unbound":  {objs: []client.Object{sourcePVC(block, corev1.ClaimPending, "10Gi")}, reason: ReasonSourceNotBound, inMsg: "Pending"},
		"in use":   {objs: []client.Object{sourcePVC(block, corev1.ClaimBound, "10Gi"), podUsing("guest-a", corev1.PodRunning)}, reason: ReasonSourceInUse, inMsg: `"guest-a"`},
		"starting": {objs: []client.Object{sourcePVC(block, corev1.ClaimBound, "10Gi"), podUsing("guest-b", corev1.PodPending)}, reason: ReasonSourceInUse, inMsg: `"guest-b"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, res, c := reconcileImage(t, append(tc.objs, pvcCloneImage(""))...)
			if got.Status.Phase != imagev1alpha1.SwiftImagePhasePending {
				t.Errorf("phase = %q, want Pending", got.Status.Phase)
			}
			cond := sourceReady(got)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.inMsg) {
				t.Errorf("SourceReady = %+v, want False %s mentioning %s", cond, tc.reason, tc.inMsg)
			}
			if res.RequeueAfter == 0 {
				t.Error("a waiting import must be retried")
			}
			var jobs batchv1.JobList
			_ = c.List(context.Background(), &jobs)
			if len(jobs.Items) != 0 {
				t.Errorf("started an import while the source is not ready: %s", jobs.Items[0].Name)
			}
		})
	}
}

// A Block source is read as a device, a Filesystem one as image.raw in its
// root; both read-only. The import PVC holds the whole source.
func TestPVCClone_ImportsTheSource(t *testing.T) {
	for _, mode := range []corev1.PersistentVolumeMode{corev1.PersistentVolumeBlock, corev1.PersistentVolumeFilesystem} {
		t.Run(string(mode), func(t *testing.T) {
			got, _, c := reconcileImage(t, pvcCloneImage("10Gi"), sourcePVC(mode, corev1.ClaimBound, "20Gi"),
				podUsing("done", corev1.PodSucceeded))
			if got.Status.Phase != imagev1alpha1.SwiftImagePhaseImporting {
				t.Fatalf("phase = %q, want Importing", got.Status.Phase)
			}
			if cond := sourceReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
				t.Errorf("SourceReady = %+v, want True", cond)
			}
			job := importJob(t, c)
			pod := job.Spec.Template.Spec
			var src *corev1.Volume
			for i := range pod.Volumes {
				if pod.Volumes[i].Name == "source" {
					src = &pod.Volumes[i]
				}
			}
			if src == nil || src.PersistentVolumeClaim == nil || src.PersistentVolumeClaim.ClaimName != "src" || !src.PersistentVolumeClaim.ReadOnly {
				t.Fatalf("source volume = %+v, want PVC src read-only", src)
			}
			ctr := pod.Containers[0]
			var path string
			for _, e := range ctr.Env {
				if e.Name == importSourcePathEnv {
					path = e.Value
				}
			}
			if mode == corev1.PersistentVolumeBlock {
				if len(ctr.VolumeDevices) != 1 || ctr.VolumeDevices[0].DevicePath != importPVCSourceDevice || path != importPVCSourceDevice {
					t.Errorf("block source: devices %+v, path %q", ctr.VolumeDevices, path)
				}
			} else {
				mounted := false
				for _, m := range ctr.VolumeMounts {
					if m.Name == "source" && m.MountPath == importPVCSourceMount && m.ReadOnly {
						mounted = true
					}
				}
				if !mounted || path != importPVCSourceMount+"/image.raw" || len(ctr.VolumeDevices) != 0 {
					t.Errorf("filesystem source: mounts %+v, path %q", ctr.VolumeMounts, path)
				}
			}
			var pvc corev1.PersistentVolumeClaim
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: importPVCNamePrefix + "img"}, &pvc); err != nil {
				t.Fatal(err)
			}
			if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(resource.MustParse("20Gi")) != 0 {
				t.Errorf("import PVC = %s, want the source's 20Gi", q.String())
			}
		})
	}
}

// The script copies with qemu-img in the declared format, checks a qcow2
// for backing and data files first, and never writes the source.
func TestImportScriptPVC(t *testing.T) {
	raw := importScriptPVC("raw", "linux")
	if !strings.Contains(raw, `qemu-img convert -f raw -O raw "$SRC" "$OUTPUT"`) || strings.Contains(raw, "backing-filename") {
		t.Errorf("raw script:\n%s", raw)
	}
	q := importScriptPVC("qcow2", "linux")
	if !strings.Contains(q, "backing-filename") || !strings.Contains(q, `qemu-img convert -f qcow2 -O raw "$SRC" "$OUTPUT"`) {
		t.Errorf("qcow2 script:\n%s", q)
	}
	if !strings.Contains(raw, `SRC="$`+importSourcePathEnv+`"`) {
		t.Errorf("source path must come from the env, not the script text:\n%s", raw)
	}
	if w := importScriptPVC("raw", "windows"); strings.Contains(w, "patch_grub") {
		t.Error("windows: no GRUB patch")
	}
}

// A pvcClone import's finished pod names the source PVC, and PVC protection
// keeps a PVC while such a pod exists: the source could not be deleted (seen
// on a lab cluster). Once the Job has succeeded its pods go; an http import's
// are kept, as before.
func TestPVCClone_FinishedImportReleasesTheSource(t *testing.T) {
	for name, src := range map[string]imagev1alpha1.ImageSource{
		"pvcClone": {PVCClone: &imagev1alpha1.PVCCloneSource{Name: "src"}},
		"http":     {HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.invalid/disk.img"}},
	} {
		t.Run(name, func(t *testing.T) {
			img := pvcCloneImage("")
			img.Spec.Source = src
			img.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting
			jobName := names.JobName(importJobNamePrefix+"img", "")
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "default"},
				Status: batchv1.JobStatus{Succeeded: 1}}
			pod := podUsing("import-pod", corev1.PodSucceeded)
			pod.Labels = map[string]string{"job-name": jobName}
			scheme := testScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img, job, pod).Build()
			r := &SwiftImageReconciler{Client: c, Scheme: scheme}
			for i := 0; i < 2; i++ { // repeatable
				phase, _, msg, err := r.CheckImportStatus(context.Background(), img)
				if err != nil || msg != "" || phase != imagev1alpha1.SwiftImagePhaseValidating {
					t.Fatalf("pass %d: phase %q msg %q err %v", i, phase, msg, err)
				}
			}
			var pods corev1.PodList
			_ = c.List(context.Background(), &pods)
			if kept := len(pods.Items) == 1; kept != (name == "http") {
				t.Errorf("%s: import pods left = %d", name, len(pods.Items))
			}
		})
	}
}
