package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
)

var candidateIdentity = buildinfo.Identity{Version: "0.7.0", SourceHash: "source", HAProxyVersion: "3.3"}

func rolloutFixture() (*Plan, *fake.Clientset, *corev1.Pod) {
	labels := map[string]string{"app": "controller"}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "test", UID: "deployment", ResourceVersion: "23", Generation: 2}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: "candidate", ImagePullPolicy: corev1.PullIfNotPresent}, {Name: "sidecar", Image: "sidecar-old"}}}},
	}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	set := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "test", UID: "set", Labels: labels, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", UID: deployment.UID, Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "test", UID: "pod", Labels: labels, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", UID: set.UID, Controller: ptr.To(true)}}}, Spec: deployment.Spec.Template.Spec, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(deployment, set, pod)
	plan := &Plan{Client: client, Deployment: deployment, Container: "controller", Image: "candidate", PullPolicy: corev1.PullIfNotPresent, Identity: candidateIdentity,
		ReadIdentity: func(context.Context, *corev1.Pod) (buildinfo.Identity, error) { return candidateIdentity, nil }}
	return plan, client, pod
}

func TestPlanUnchangedReferenceRequiresActualBinaryIdentity(t *testing.T) {
	for _, identity := range []buildinfo.Identity{{}, {Version: "0.6.0", SourceHash: "old", HAProxyVersion: "3.3"}, {Version: "0.7.0", SourceHash: "source", HAProxyVersion: "3.2"}} {
		plan, client, _ := rolloutFixture()
		plan.ReadIdentity = func(context.Context, *corev1.Pod) (buildinfo.Identity, error) { return identity, nil }
		require.ErrorContains(t, plan.Run(t.Context()), "new immutable tag or digest")
		for _, action := range client.Actions() {
			assert.NotEqual(t, "patch", action.GetVerb())
		}
	}
}

func TestPlanPreservesSidecarAndUsesDeploymentCAS(t *testing.T) {
	plan, client, _ := rolloutFixture()
	plan.Image = "new-candidate"
	client.PrependReactor("patch", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		patch := action.(ktesting.PatchAction)
		var body map[string]any
		require.NoError(t, json.Unmarshal(patch.GetPatch(), &body))
		assert.Equal(t, map[string]any{"uid": "deployment", "resourceVersion": "23"}, body["metadata"])
		assert.Equal(t, map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "controller", "image": "new-candidate", "imagePullPolicy": "IfNotPresent"}}}}}, body["spec"])
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "controller", errors.New("concurrent change"))
	})
	require.ErrorContains(t, plan.Run(t.Context()), "concurrent change")
}

func TestPodsReadyKeepsTerminatingWatchersAndIgnoresForeignOwners(t *testing.T) {
	plan, client, pod := rolloutFixture()
	owners := map[types.UID]bool{}
	ready, err := plan.podsReady(t.Context(), plan.Deployment, owners, false)
	require.NoError(t, err)
	require.True(t, ready)
	old := pod.DeepCopy()
	old.Name, old.UID = "terminating", "terminating"
	old.DeletionTimestamp = ptr.To(metav1.Now())
	old.Finalizers = []string{"test"}
	_, err = client.CoreV1().Pods("test").Create(t.Context(), old, metav1.CreateOptions{})
	require.NoError(t, err)
	ready, err = plan.podsReady(t.Context(), plan.Deployment, owners, false)
	require.NoError(t, err)
	assert.False(t, ready, "terminating old processes can still watch templates")
	require.NoError(t, client.CoreV1().Pods("test").Delete(t.Context(), old.Name, metav1.DeleteOptions{}))
	foreign := pod.DeepCopy()
	foreign.Name, foreign.UID = "foreign", "foreign"
	foreign.OwnerReferences[0].UID = "unrelated-set"
	foreign.Spec.Containers[0].Image = "old"
	_, err = client.CoreV1().Pods("test").Create(t.Context(), foreign, metav1.CreateOptions{})
	require.NoError(t, err)
	ready, err = plan.podsReady(t.Context(), plan.Deployment, owners, false)
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestPlanIdentityTransportFailureNeverSucceeds(t *testing.T) {
	plan, _, _ := rolloutFixture()
	plan.ReadIdentity = func(context.Context, *corev1.Pod) (buildinfo.Identity, error) {
		return buildinfo.Identity{}, errors.New("forbidden")
	}
	require.ErrorContains(t, plan.Run(t.Context()), "forbidden")
}

func TestPlanRejectsReplacedDeployment(t *testing.T) {
	plan, client, _ := rolloutFixture()
	replacement := plan.Deployment.DeepCopy()
	replacement.UID = "replacement"
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), replacement, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, plan.Run(t.Context()), "replaced")
}

func TestPlanUsesDesiredAvailabilityAndGeneration(t *testing.T) {
	plan, client, _ := rolloutFixture()
	deployment := plan.Deployment.DeepCopy()
	deployment.Status.AvailableReplicas = 0
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { cancel(); return false, nil, nil })
	require.ErrorIs(t, plan.Run(ctx), context.Canceled)
}

func TestPlanRejectsSameImageDeploymentMutation(t *testing.T) {
	plan, client, _ := rolloutFixture()
	current := plan.Deployment.DeepCopy()
	current.Generation++
	current.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CRD_NAME", Value: "different-config"}}
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), current, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, plan.Run(t.Context()), "changed during upgrade preparation")
}

func TestPlanRetainsOldPodIdentityAfterReplicaSetDeletion(t *testing.T) {
	plan, client, pod := rolloutFixture()
	require.NoError(t, plan.captureOwnership(t.Context()))
	require.NoError(t, client.AppsV1().ReplicaSets("test").Delete(t.Context(), "set", metav1.DeleteOptions{}))
	pod.Labels = nil
	pod.DeletionTimestamp = ptr.To(metav1.Now())
	_, err := client.CoreV1().Pods("test").Update(t.Context(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	ready, err := plan.podsReady(t.Context(), plan.Deployment, plan.owners, false)
	require.NoError(t, err)
	assert.False(t, ready, "an old watcher cannot disappear through ReplicaSet pruning or relabeling")
}

func TestRecoveryCannotAcceptAnUnchangedTagWithOldNotReadyBinary(t *testing.T) {
	plan, client, pod := rolloutFixture()
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err := client.CoreV1().Pods("test").Update(t.Context(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	plan.ReadIdentity = func(context.Context, *corev1.Pod) (buildinfo.Identity, error) { return buildinfo.Identity{}, nil }
	require.ErrorContains(t, plan.VerifyUnchangedImage(t.Context()), "new immutable tag or digest")
	for _, action := range client.Actions() {
		assert.NotEqual(t, "patch", action.GetVerb())
	}
}

func TestRecoveryAcceptsSameBuildIdentityFromNotReadyController(t *testing.T) {
	plan, client, pod := rolloutFixture()
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	_, err := client.CoreV1().Pods("test").Update(t.Context(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, plan.VerifyUnchangedImage(t.Context()))
	for _, action := range client.Actions() {
		assert.NotEqual(t, "patch", action.GetVerb())
	}
}

func TestRecoveryRequiresActualBuildProofForUnchangedImage(t *testing.T) {
	plan, client, pod := rolloutFixture()
	require.NoError(t, client.CoreV1().Pods("test").Delete(t.Context(), pod.Name, metav1.DeleteOptions{}))
	require.ErrorContains(t, plan.VerifyUnchangedImage(t.Context()), "cannot be verified for all desired controller replicas")
}

func TestZeroReplicaDeploymentNeedsNoImageMutation(t *testing.T) {
	plan, client, pod := rolloutFixture()
	plan.Deployment.Spec.Paused = true
	plan.Deployment.Spec.Replicas = ptr.To(int32(0))
	plan.Deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: plan.Deployment.Generation}
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), plan.Deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, client.CoreV1().Pods("test").Delete(t.Context(), pod.Name, metav1.DeleteOptions{}))
	plan.Image = "new"
	require.NoError(t, plan.Run(t.Context()))
	for _, action := range client.Actions() {
		assert.NotEqual(t, "patch", action.GetVerb())
	}
}

func TestExitedControllerPodsAreNotWatchers(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		plan, client, pod := rolloutFixture()
		evicted := pod.DeepCopy()
		evicted.Name, evicted.UID = "evicted", "evicted"
		evicted.Spec.Containers[0].Image = "old"
		evicted.Status = corev1.PodStatus{Phase: phase}
		_, err := client.CoreV1().Pods("test").Create(t.Context(), evicted, metav1.CreateOptions{})
		require.NoError(t, err)
		plan.ReadIdentity = func(_ context.Context, target *corev1.Pod) (buildinfo.Identity, error) {
			if target.UID == evicted.UID {
				return buildinfo.Identity{}, errors.New("exited Pod has no address")
			}
			return candidateIdentity, nil
		}
		require.NoError(t, plan.VerifyUnchangedImage(t.Context()), phase)
		ready, err := plan.podsReady(t.Context(), plan.Deployment, map[types.UID]bool{}, false)
		require.NoError(t, err, phase)
		assert.True(t, ready, phase)
	}
}

func TestPlanReleasesImageOwnershipToTheReleaseTool(t *testing.T) {
	plan, client, _ := rolloutFixture()
	plan.Deployment.ManagedFields = []metav1.ManagedFieldsEntry{
		{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply},
		{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationUpdate},
	}
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), plan.Deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	var patchManager string
	client.PrependReactor("patch", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchManager = action.(ktesting.PatchActionImpl).PatchOptions.FieldManager
		return false, nil, nil
	})
	plan.Image = "new-candidate"
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_ = plan.Run(ctx)
	assert.Equal(t, FieldManager, patchManager)
	deployment, err := client.AppsV1().Deployments("test").Get(t.Context(), "controller", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "new-candidate", deployment.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, []metav1.ManagedFieldsEntry{{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply}}, deployment.ManagedFields,
		"a retained owner conflicts with the release tool's next apply, including rollback")
}

func TestReleaseOwnershipRetriesAConcurrentDeploymentWrite(t *testing.T) {
	plan, client, _ := rolloutFixture()
	plan.Deployment.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "helm"}, {Manager: FieldManager}}
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), plan.Deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	attempts := 0
	client.PrependReactor("patch", "deployments", func(action ktesting.Action) (bool, runtime.Object, error) {
		var ops []map[string]any
		require.NoError(t, json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &ops))
		assert.Equal(t, map[string]any{"op": "replace", "path": "/metadata/resourceVersion", "value": ops[0]["value"]}, ops[0],
			"only a resourceVersion precondition turns a concurrent status write into a retryable Conflict")
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "controller", errors.New("status updated"))
		}
		return false, nil, nil
	})
	require.NoError(t, plan.releaseOwnership(t.Context()))
	assert.Equal(t, 2, attempts)
	deployment, err := client.AppsV1().Deployments("test").Get(t.Context(), "controller", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []metav1.ManagedFieldsEntry{{Manager: "helm"}}, deployment.ManagedFields)
}

func TestRetriedPreparationWaitsForItsOwnInterruptedRollout(t *testing.T) {
	plan, client, pod := rolloutFixture()
	plan.Deployment.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "helm"}, {Manager: FieldManager}}
	_, err := client.AppsV1().Deployments("test").Update(t.Context(), plan.Deployment, metav1.UpdateOptions{})
	require.NoError(t, err)
	old := pod.DeepCopy()
	old.Name, old.UID = "old", "old"
	old.Spec.Containers[0].Image = "released"
	_, err = client.CoreV1().Pods("test").Create(t.Context(), old, metav1.CreateOptions{})
	require.NoError(t, err)
	plan.ReadIdentity = func(_ context.Context, target *corev1.Pod) (buildinfo.Identity, error) {
		if target.UID == old.UID {
			return buildinfo.Identity{}, nil
		}
		return candidateIdentity, nil
	}
	require.NoError(t, plan.VerifyUnchangedImage(t.Context()), "a Pod still on the released image is a rollout in progress, not a retag")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, plan.Run(ctx), context.DeadlineExceeded, "the old watcher keeps preparation waiting")
	deployment, err := client.AppsV1().Deployments("test").Get(t.Context(), "controller", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []metav1.ManagedFieldsEntry{{Manager: "helm"}}, deployment.ManagedFields)
}
