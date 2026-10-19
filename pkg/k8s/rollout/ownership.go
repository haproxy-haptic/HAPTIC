package rollout

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

const replicaSetKind = "ReplicaSet"

func (p *Plan) captureOwnership(ctx context.Context) error {
	if p.owners == nil {
		p.owners = make(map[types.UID]bool)
	}
	if p.pods == nil {
		p.pods = make(map[types.UID]bool)
	}
	sets, err := p.Client.AppsV1().ReplicaSets(p.Deployment.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	knownSets := make(map[types.UID]bool, len(sets.Items))
	for i := range sets.Items {
		set := &sets.Items[i]
		knownSets[set.UID] = true
		if owner := metav1.GetControllerOf(set); owner != nil && owner.Kind == "Deployment" && owner.UID == p.Deployment.UID {
			p.owners[set.UID] = true
		}
	}
	pods, err := p.Client.CoreV1().Pods(p.Deployment.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	selector, err := metav1.LabelSelectorAsSelector(p.Deployment.Spec.Selector)
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if exited(pod) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.Kind == replicaSetKind && !knownSets[owner.UID] && !p.owners[owner.UID] && selector.Matches(labels.Set(pod.Labels)) {
			return fmt.Errorf("cannot establish ownership of controller Pod %s; wait for its termination before upgrading", pod.Name)
		}
		if owner := metav1.GetControllerOf(pod); owner != nil && owner.Kind == replicaSetKind && p.owners[owner.UID] {
			p.pods[pod.UID] = true
		}
	}
	return nil
}

// exited Pods have no running containers, so they cannot watch templates.
func exited(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// VerifyUnchangedImage prevents a retagged image from bypassing rollout during recovery.
func (p *Plan) VerifyUnchangedImage(ctx context.Context) error {
	current := Container(p.Deployment.Spec.Template.Spec.Containers, p.Container)
	if current == nil || current.Image != p.Image || replicas(p.Deployment) == 0 {
		return nil
	}
	if err := p.captureOwnership(ctx); err != nil {
		return err
	}
	pods, err := p.Client.CoreV1().Pods(p.Deployment.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if container := Container(pod.Spec.Containers, p.Container); p.pods[pod.UID] && (container == nil || container.Image != p.Image) {
			// An interrupted preparation already changed the image; Run's wait proves each replacement.
			return nil
		}
	}
	verified := int32(0)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !p.pods[pod.UID] {
			continue
		}
		if err := p.requireIdentity(ctx, pod, true); err != nil {
			return err
		}
		if pod.DeletionTimestamp == nil {
			verified++
		}
	}
	if verified < replicas(p.Deployment) {
		return fmt.Errorf("image %s cannot be verified for all desired controller replicas; use a new immutable tag or digest before upgrading", p.Image)
	}
	return nil
}

func (p *Plan) requireIdentity(ctx context.Context, pod *corev1.Pod, unchangedImage bool) error {
	identity, err := p.ReadIdentity(ctx, pod)
	if err != nil {
		return fmt.Errorf("read controller build identity from Pod %s: %w", pod.Name, err)
	}
	if identity == p.Identity {
		return nil
	}
	if unchangedImage {
		return fmt.Errorf("image %s resolves to a different controller build in Pod %s; use a new immutable tag or digest before upgrading", p.Image, pod.Name)
	}
	return fmt.Errorf("controller Pod %s runs a different controller build from preflight; correct the image tag or pull policy", pod.Name)
}
