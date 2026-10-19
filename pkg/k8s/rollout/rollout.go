package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
)

// FieldManager names the image patch so its ownership entry can be released.
const FieldManager = "haptic-prepare-upgrade"

// IdentityReader reads the running binary, rather than trusting its image tag.
type IdentityReader func(context.Context, *corev1.Pod) (buildinfo.Identity, error)

// Plan changes one container while preserving the Deployment's rollout policy.
type Plan struct {
	Client       kubernetes.Interface
	Deployment   *appsv1.Deployment
	Container    string
	Image        string
	PullPolicy   corev1.PullPolicy
	Identity     buildinfo.Identity
	ReadIdentity IdentityReader
	owners       map[types.UID]bool
	pods         map[types.UID]bool
}

func (p *Plan) Run(ctx context.Context) error {
	if err := p.Identity.Validate(); err != nil {
		return err
	}
	if err := p.captureOwnership(ctx); err != nil {
		return err
	}
	current := Container(p.Deployment.Spec.Template.Spec.Containers, p.Container)
	if current == nil {
		return fmt.Errorf("controller Deployment %s has no container %s", p.Deployment.Name, p.Container)
	}
	if p.Deployment.Spec.Paused && replicas(p.Deployment) > 0 {
		return fmt.Errorf("controller Deployment %s is paused; resume it before preparing the upgrade", p.Deployment.Name)
	}
	if current.Image == p.Image && current.ImagePullPolicy == p.PullPolicy {
		// A retried preparation finds its own patch applied but possibly still owned.
		if err := p.releaseOwnership(ctx); err != nil {
			return err
		}
		return p.wait(ctx, true)
	}
	if replicas(p.Deployment) == 0 {
		return p.wait(ctx, false)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": p.Deployment.UID, "resourceVersion": p.Deployment.ResourceVersion},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []map[string]any{{"name": p.Container, "image": p.Image, "imagePullPolicy": p.PullPolicy}}}}},
	})
	if err != nil {
		return err
	}
	updated, err := p.Client.AppsV1().Deployments(p.Deployment.Namespace).Patch(ctx, p.Deployment.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager})
	if err != nil {
		return fmt.Errorf("prepare Deployment %s: %w", p.Deployment.Name, err)
	}
	p.Deployment = updated
	if err := p.releaseOwnership(ctx); err != nil {
		return err
	}
	return p.wait(ctx, false)
}

// releaseOwnership drops the patch's managedFields entry: a retained owner of the
// old value makes the release tool's next server-side apply, such as a rollback, conflict.
func (p *Plan) releaseOwnership(ctx context.Context) error {
	deployments := p.Client.AppsV1().Deployments(p.Deployment.Namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := deployments.Get(ctx, p.Deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != p.Deployment.UID {
			return fmt.Errorf("controller Deployment %s was replaced during preparation", p.Deployment.Name)
		}
		kept := make([]metav1.ManagedFieldsEntry, 0, len(current.ManagedFields))
		for _, entry := range current.ManagedFields {
			if entry.Manager != FieldManager {
				kept = append(kept, entry)
			}
		}
		// An empty list means "unchanged" to the API server; with no other owner there is no conflict.
		if len(kept) == len(current.ManagedFields) || len(kept) == 0 {
			return nil
		}
		// A resourceVersion in the patched object makes a concurrent write a retryable
		// Conflict; a failed JSON Patch "test" op is a non-retryable Invalid.
		patch, err := json.Marshal([]map[string]any{
			{"op": "replace", "path": "/metadata/resourceVersion", "value": current.ResourceVersion},
			{"op": "replace", "path": "/metadata/managedFields", "value": kept},
		})
		if err != nil {
			return err
		}
		updated, err := deployments.Patch(ctx, current.Name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: FieldManager})
		if err != nil {
			return err
		}
		p.Deployment = updated
		return nil
	})
	if err != nil {
		return fmt.Errorf("release image ownership on Deployment %s: %w", p.Deployment.Name, err)
	}
	return nil
}

func (p *Plan) wait(ctx context.Context, unchangedImage bool) error {
	owners := p.owners
	if owners == nil {
		owners = make(map[types.UID]bool)
	}
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		deployment, err := p.Client.AppsV1().Deployments(p.Deployment.Namespace).Get(ctx, p.Deployment.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deployment.UID != p.Deployment.UID {
			return false, errors.New("controller Deployment was replaced during upgrade preparation")
		}
		if deployment.Generation != p.Deployment.Generation {
			return false, errors.New("controller Deployment changed during upgrade preparation; retry against its current specification")
		}
		container := Container(deployment.Spec.Template.Spec.Containers, p.Container)
		if container == nil || (replicas(deployment) > 0 && (container.Image != p.Image || container.ImagePullPolicy != p.PullPolicy)) {
			return false, errors.New("controller image changed during upgrade preparation")
		}
		ready, err := p.podsReady(ctx, deployment, owners, unchangedImage)
		if err != nil || !ready {
			return false, err
		}
		desired := replicas(deployment)
		return deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.UpdatedReplicas == desired && deployment.Status.Replicas == desired && deployment.Status.AvailableReplicas == desired, nil
	})
	if err != nil {
		return fmt.Errorf("controller preparation did not complete; incoming templates remain unapplied: %w", err)
	}
	return nil
}

func (p *Plan) podsReady(ctx context.Context, deployment *appsv1.Deployment, owners map[types.UID]bool, unchangedImage bool) (bool, error) {
	options := metav1.ListOptions{}
	sets, err := p.Client.AppsV1().ReplicaSets(deployment.Namespace).List(ctx, options)
	if err != nil {
		return false, err
	}
	for i := range sets.Items {
		set := &sets.Items[i]
		if owner := metav1.GetControllerOf(set); owner != nil && owner.Kind == "Deployment" && owner.UID == deployment.UID {
			owners[set.UID] = true
		}
	}
	pods, err := p.Client.CoreV1().Pods(deployment.Namespace).List(ctx, options)
	if err != nil {
		return false, err
	}
	count := int32(0)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if exited(pod) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if !p.pods[pod.UID] && (owner == nil || owner.Kind != replicaSetKind || !owners[owner.UID]) {
			continue
		}
		count++
		ready, err := p.podReady(ctx, pod, unchangedImage)
		if err != nil || !ready {
			return false, err
		}
	}
	return count == replicas(deployment), nil
}

func (p *Plan) podReady(ctx context.Context, pod *corev1.Pod, unchangedImage bool) (bool, error) {
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	container := Container(pod.Spec.Containers, p.Container)
	if container == nil || container.Image != p.Image || !podReady(pod) {
		return false, nil
	}
	if err := p.requireIdentity(ctx, pod, unchangedImage); err != nil {
		return false, err
	}
	return true, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func replicas(deployment *appsv1.Deployment) int32 {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return *deployment.Spec.Replicas
}

func Container(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}
