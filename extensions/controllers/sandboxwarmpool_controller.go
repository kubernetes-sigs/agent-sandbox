package extensioncontrollers

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	poolLabel               = "agents.x-k8s.io/pool"
	conditionTypeInProgress = "InProgress"
	conditionTypeCurrent    = "Current"
)

// SandboxWarmPoolReconciler reconciles a SandboxWarmPool object
type SandboxWarmPoolReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the reconciliation loop for SandboxWarmPool
func (r *SandboxWarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the SandboxWarmPool instance
	warmPool := &extensionsv1alpha1.SandboxWarmPool{}
	if err := r.Get(ctx, req.NamespacedName, warmPool); err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info("SandboxWarmPool resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get SandboxWarmPool")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !warmPool.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("SandboxWarmPool is being deleted")
		return ctrl.Result{}, nil
	}

	// Save old status for comparison
	oldStatus := warmPool.Status.DeepCopy()

	// Reconcile the pool (create Sandboxes as needed)
	reconcileErr := r.reconcilePool(ctx, warmPool)

	// Update status if it has changed
	if err := r.updateStatus(ctx, oldStatus, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return ctrl.Result{}, err
	}

	// Return any reconciliation errors
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}

	return ctrl.Result{}, nil
}

// reconcilePool ensures the correct number of sandboxes exist in the pool
func (r *SandboxWarmPoolReconciler) reconcilePool(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Get the pool label value (pool-name-hash)
	poolLabelValue := r.getPoolLabelValue(warmPool)

	// List all sandboxes with the pool label
	sandboxList := &sandboxv1alpha1.SandboxList{}
	labelSelector := labels.SelectorFromSet(labels.Set{
		poolLabel: poolLabelValue,
	})

	if err := r.List(ctx, sandboxList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     warmPool.Namespace,
	}); err != nil {
		log.Error(err, "Failed to list sandboxes")
		return err
	}

	desiredReplicas := warmPool.Spec.Replicas
	currentReplicas := int32(len(sandboxList.Items))
	readyReplicas := r.countReadySandboxes(sandboxList.Items)

	log.Info("Pool status",
		"desired", desiredReplicas,
		"current", currentReplicas,
		"ready", readyReplicas,
		"poolLabel", poolLabelValue)

	// Update status replicas and readyReplicas
	warmPool.Status.Replicas = currentReplicas
	warmPool.Status.ReadyReplicas = readyReplicas

	var allErrors error

	// Create new sandboxes if we need more
	if currentReplicas < desiredReplicas {
		sandboxesToCreate := desiredReplicas - currentReplicas
		log.Info("Creating new sandboxes",
			"count", sandboxesToCreate,
			"desired", desiredReplicas,
			"current", currentReplicas)

		for i := int32(0); i < sandboxesToCreate; i++ {
			if err := r.createPoolSandbox(ctx, warmPool, poolLabelValue); err != nil {
				log.Error(err, "Failed to create sandbox")
				allErrors = errors.Join(allErrors, err)
			}
		}
	}

	// Delete excess sandboxes if we have too many
	if currentReplicas > desiredReplicas {
		sandboxesToDelete := currentReplicas - desiredReplicas
		log.Info("Deleting excess sandboxes",
			"count", sandboxesToDelete,
			"desired", desiredReplicas,
			"current", currentReplicas)

		// Delete the specified number of sandboxes
		for i := int32(0); i < sandboxesToDelete && i < int32(len(sandboxList.Items)); i++ {
			sandbox := &sandboxList.Items[i]
			if err := r.Delete(ctx, sandbox); err != nil && !k8serrors.IsNotFound(err) {
				log.Error(err, "Failed to delete sandbox", "sandbox", sandbox.Name)
				allErrors = errors.Join(allErrors, err)
			} else {
				log.Info("Deleted excess sandbox", "sandbox", sandbox.Name)
			}
		}
	}

	return allErrors
}

// createPoolSandbox creates a new sandbox for the warm pool
func (r *SandboxWarmPoolReconciler) createPoolSandbox(ctx context.Context, warmPool *extensionsv1alpha1.SandboxWarmPool, poolLabelValue string) error {
	log := log.FromContext(ctx)

	// Generate a unique name for the sandbox: <pool-name>-<random-suffix>
	sandboxName := fmt.Sprintf("%s-%s", warmPool.Name, rand.String(5))

	// Create labels for the sandbox
	sandboxLabels := make(map[string]string)
	sandboxLabels[poolLabel] = poolLabelValue

	// Copy labels from pod template
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Labels {
		sandboxLabels[k] = v
	}

	// Create annotations for the sandbox
	sandboxAnnotations := make(map[string]string)
	for k, v := range warmPool.Spec.PodTemplate.ObjectMeta.Annotations {
		sandboxAnnotations[k] = v
	}

	// Create the sandbox
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandboxName,
			Namespace:   warmPool.Namespace,
			Labels:      sandboxLabels,
			Annotations: sandboxAnnotations,
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: warmPool.Spec.PodTemplate,
		},
	}

	// // Set GVK for the Sandbox
	// sandbox.SetGroupVersionKind(sandboxv1alpha1.GroupVersion.WithKind("Sandbox"))

	// Set controller reference so the Sandbox is owned by the SandboxWarmPool
	if err := ctrl.SetControllerReference(warmPool, sandbox, r.Client.Scheme()); err != nil {
		return fmt.Errorf("SetControllerReference for Sandbox failed: %w", err)
	}

	// Create the Sandbox
	if err := r.Create(ctx, sandbox); err != nil {
		log.Error(err, "Failed to create sandbox", "sandbox", sandboxName)
		return err
	}

	log.Info("Created new pool sandbox", "sandbox", sandboxName, "pool", poolLabelValue)
	return nil
}

// getPoolLabelValue generates the pool label value: <pool-name>-<hash>
func (r *SandboxWarmPoolReconciler) getPoolLabelValue(warmPool *extensionsv1alpha1.SandboxWarmPool) string {
	hash := NameHash(warmPool.Name)
	return fmt.Sprintf("%s-%s", warmPool.Name, hash)
}

// countReadySandboxes counts the number of ready sandboxes in the list
func (r *SandboxWarmPoolReconciler) countReadySandboxes(sandboxes []sandboxv1alpha1.Sandbox) int32 {
	count := int32(0)
	for _, sandbox := range sandboxes {
		for _, condition := range sandbox.Status.Conditions {
			if condition.Type == string(sandboxv1alpha1.SandboxConditionReady) && condition.Status == metav1.ConditionTrue {
				count++
				break
			}
		}
	}
	return count
}

// updateStatus updates the status of the SandboxWarmPool if it has changed
func (r *SandboxWarmPoolReconciler) updateStatus(ctx context.Context, oldStatus *extensionsv1alpha1.SandboxWarmPoolStatus, warmPool *extensionsv1alpha1.SandboxWarmPool) error {
	log := log.FromContext(ctx)

	// Check if status has changed
	if oldStatus.Replicas == warmPool.Status.Replicas &&
		oldStatus.ReadyReplicas == warmPool.Status.ReadyReplicas {
		return nil
	}

	if err := r.Status().Update(ctx, warmPool); err != nil {
		log.Error(err, "Failed to update SandboxWarmPool status")
		return err
	}

	log.Info("Updated SandboxWarmPool status",
		"replicas", warmPool.Status.Replicas,
		"readyReplicas", warmPool.Status.ReadyReplicas)
	return nil
}

// NameHash generates an FNV-1a hash from a string and returns
// it as a fixed-length hexadecimal string.
func NameHash(objectName string) string {
	h := fnv.New32a()
	h.Write([]byte(objectName))
	hashValue := h.Sum32()

	// Convert the uint32 to a hexadecimal string.
	// This results in an 8-character string (e.g., "a5b3c2d1").
	return fmt.Sprintf("%08x", hashValue)
}

// SetupWithManager sets up the controller with the Manager
func (r *SandboxWarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.SandboxWarmPool{}).
		Owns(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}
