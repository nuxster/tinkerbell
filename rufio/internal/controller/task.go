/*
Copyright 2022 Tinkerbell.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	bmclib "github.com/bmc-toolbox/bmclib/v2"
	"github.com/bmc-toolbox/bmclib/v2/constants"
	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
)

const (
	powerActionRequeueAfter = 3 * time.Second

	// firmwarePollInterval/firmwarePollTimeout bound the inline poll of a
	// firmware install task to a terminal state.
	firmwarePollInterval = 5 * time.Second
	firmwarePollTimeout  = 9 * time.Minute
)

// imageFetcher fetches a firmware image from a URL, returning a reader the
// caller must close. It is a field so tests can inject a hermetic stub.
type imageFetcher func(ctx context.Context, url string) (io.ReadCloser, error)

// TaskReconciler reconciles a Task object.
type TaskReconciler struct {
	client           client.Client
	bmcClientFactory ClientFunc
	fetchImage       imageFetcher
}

// TaskOption customizes a TaskReconciler.
type TaskOption func(*TaskReconciler)

// WithImageFetcher overrides the firmware image fetcher (used in tests to avoid
// real network access).
func WithImageFetcher(f func(ctx context.Context, url string) (io.ReadCloser, error)) TaskOption {
	return func(r *TaskReconciler) { r.fetchImage = f }
}

// NewTaskReconciler returns a new TaskReconciler.
func NewTaskReconciler(c client.Client, bmcClientFactory ClientFunc, opts ...TaskOption) *TaskReconciler {
	r := &TaskReconciler{
		client:           c,
		bmcClientFactory: bmcClientFactory,
		fetchImage:       httpImageFetcher,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// httpImageFetcher is the default firmware image fetcher: a plain HTTP GET.
func httpImageFetcher(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d fetching firmware image", resp.StatusCode)
	}
	return resp.Body, nil
}

// isCapabilityUnsupported reports whether err indicates that no connected
// provider implements the requested capability (bmclib returns
// "no <Interface> implementations found"). Such an error is mapped to a clear
// Task condition rather than treated as an infrastructure failure.
func isCapabilityUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "implementations found") || strings.Contains(msg, "not supported")
}

//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=tasks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=tasks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=tasks/finalizers,verbs=update

// Reconcile runs a Task.
// Establishes a connection to the BMC.
// Runs the specified action in the Task.
func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("controllers/Task").WithValues("task", req.NamespacedName)
	logger.Info("Reconciling Task")

	// Fetch the Task object
	task := &bmc.Task{}
	if err := r.client.Get(ctx, req.NamespacedName, task); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get Task")
		return ctrl.Result{}, err
	}

	// Deletion is a noop.
	if !task.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Task is Completed or Failed is noop.
	if task.HasCondition(bmc.TaskFailed, bmc.ConditionTrue) ||
		task.HasCondition(bmc.TaskCompleted, bmc.ConditionTrue) {
		return ctrl.Result{}, nil
	}

	// Create a patch from the initial Task object
	// Patch is used to update Status after reconciliation
	taskPatch := client.MergeFrom(task.DeepCopy())
	logger = logger.WithValues("action", task.Spec.Task, "host", task.Spec.Connection.Host)

	return r.doReconcile(ctx, task, taskPatch, logger)
}

func (r *TaskReconciler) doReconcile(ctx context.Context, task *bmc.Task, taskPatch client.Patch, logger logr.Logger) (ctrl.Result, error) {
	var username, password string
	opts := &BMCOptions{
		ProviderOptions: task.Spec.Connection.ProviderOptions,
	}
	if task.Spec.Connection.ProviderOptions != nil && task.Spec.Connection.ProviderOptions.RPC != nil {
		opts.ProviderOptions = task.Spec.Connection.ProviderOptions
		if task.Spec.Connection.ProviderOptions.RPC.HMAC != nil && len(task.Spec.Connection.ProviderOptions.RPC.HMAC.Secrets) > 0 {
			se, err := retrieveHMACSecrets(ctx, r.client, task.Spec.Connection.ProviderOptions.RPC.HMAC.Secrets)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("unable to get hmac secrets: %w", err)
			}
			opts.rpcSecrets = se
		}
	} else {
		// Fetching username, password from SecretReference in Connection.
		// Requeue if error fetching secret
		var err error
		username, password, err = resolveAuthSecretRef(ctx, r.client, task.Spec.Connection.AuthSecretRef)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving connection secret for task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}

	// Initializing BMC Client
	bmcClient, err := r.bmcClientFactory(ctx, logger, task.Spec.Connection.Host, username, password, opts)
	if err != nil {
		logger.Error(err, "BMC connection failed", "host", task.Spec.Connection.Host)
		task.SetCondition(bmc.TaskFailed, bmc.ConditionTrue, bmc.WithTaskConditionMessage(fmt.Sprintf("Failed to connect to BMC: %v", err)))
		patchErr := r.patchStatus(ctx, task, taskPatch)
		if patchErr != nil {
			return ctrl.Result{}, utilerrors.NewAggregate([]error{patchErr, err})
		}

		return ctrl.Result{}, err
	}
	defer func() {
		// Close BMC connection after reconciliation
		if err := bmcClient.Close(ctx); err != nil {
			md := bmcClient.GetMetadata()
			logger.Error(err, "BMC close connection failed", "providersAttempted", md.ProvidersAttempted)

			return
		}
		md := bmcClient.GetMetadata()
		logger.Info("BMC connection closed", "successfulCloseConns", md.SuccessfulCloseConns, "providersAttempted", md.ProvidersAttempted, "successfulProvider", md.SuccessfulProvider)
	}()

	// Task has StartTime, we check the status.
	// Requeue if actions did not complete.
	if !task.Status.StartTime.IsZero() {
		jobRunningTime := time.Since(task.Status.StartTime.Time)
		// TODO(pokearu): add timeout for tasks on API spec
		if jobRunningTime >= 10*time.Minute {
			timeOutErr := fmt.Errorf("bmc task timeout: %d", jobRunningTime)
			// Set Task Condition Failed True
			task.SetCondition(bmc.TaskFailed, bmc.ConditionTrue, bmc.WithTaskConditionMessage(timeOutErr.Error()))
			patchErr := r.patchStatus(ctx, task, taskPatch)
			if patchErr != nil {
				return ctrl.Result{}, utilerrors.NewAggregate([]error{patchErr, timeOutErr})
			}

			return ctrl.Result{}, timeOutErr
		}

		result, err := r.checkTaskStatus(ctx, logger, task.Spec.Task, bmcClient)
		if err != nil {
			return result, fmt.Errorf("bmc task status check: %w", err)
		}

		if !result.IsZero() {
			return result, nil
		}

		// Set the Task CompletionTime
		now := metav1.Now()
		task.Status.CompletionTime = &now
		// Set Task Condition Completed True
		task.SetCondition(bmc.TaskCompleted, bmc.ConditionTrue)
		if err := r.patchStatus(ctx, task, taskPatch); err != nil {
			return result, err
		}

		return result, nil
	}

	logger.Info("new task run")

	// Set the Task StartTime
	now := metav1.Now()
	task.Status.StartTime = &now
	// run the specified Task in Task
	actionResult, err := r.runTask(ctx, logger, task.Spec.Task, bmcClient)
	if err != nil {
		md := bmcClient.GetMetadata()
		logger.Info("failed to perform action", "providersAttempted", md.ProvidersAttempted, "action", task.Spec.Task)
		// Record any partial result (e.g. a firmware task id/state) alongside the failure.
		mergeResult(task, actionResult)
		// Set Task Condition Failed True
		task.SetCondition(bmc.TaskFailed, bmc.ConditionTrue, bmc.WithTaskConditionMessage(err.Error()))
		patchErr := r.patchStatus(ctx, task, taskPatch)
		if patchErr != nil {
			return ctrl.Result{}, utilerrors.NewAggregate([]error{patchErr, err})
		}

		return ctrl.Result{}, err
	}

	// Record action output (inventory summary, firmware task id/state, etc.).
	mergeResult(task, actionResult)

	if err := r.patchStatus(ctx, task, taskPatch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// runTask executes the action defined in a Task against the BMC. Exactly one
// action field is set (enforced by the CRD's MaxProperties:=1); the matching
// branch is dispatched. It returns an optional result map recorded in the Task
// status (e.g. an inventory summary or a firmware task id/state) and an error.
func (r *TaskReconciler) runTask(ctx context.Context, logger logr.Logger, task bmc.Action, bmcClient *bmclib.Client) (map[string]string, error) {
	if task.PowerAction != nil {
		ok, err := bmcClient.SetPowerState(ctx, string(*task.PowerAction))
		if err != nil {
			return nil, fmt.Errorf("failed to perform PowerAction: %w", err)
		}
		md := bmcClient.GetMetadata()
		logger.Info("power state set successfully", "providersAttempted", md.ProvidersAttempted, "successfulProvider", md.SuccessfulProvider, "ok", ok)

		return nil, nil
	}

	if task.OneTimeBootDeviceAction != nil { //nolint:staticcheck // oneTimeBootDeviceAction is deprecated but still supported for backward compatibility. We will remove in a future release.
		// OneTimeBootDeviceAction currently sets the first boot device from Devices.
		// setPersistent is false.
		ok, err := bmcClient.SetBootDevice(ctx, string(task.OneTimeBootDeviceAction.Devices[0]), false, task.OneTimeBootDeviceAction.EFIBoot) //nolint:staticcheck // oneTimeBootDeviceAction is deprecated but still supported for backward compatibility. We will remove in a future release.
		if err != nil {
			return nil, fmt.Errorf("failed to perform OneTimeBootDeviceAction: %w", err)
		}
		md := bmcClient.GetMetadata()
		logger.Info("one time boot device set successfully", "notice", "oneTimeBootDeviceAction is deprecated and will be remove in a future release. Please use bootDevice instead.", "providersAttempted", md.ProvidersAttempted, "successfulProvider", md.SuccessfulProvider, "ok", ok)

		return nil, nil
	}

	if task.BootDevice != nil {
		ok, err := bmcClient.SetBootDevice(ctx, task.BootDevice.Device.String(), task.BootDevice.Persistent, task.BootDevice.EFIBoot)
		if err != nil || !ok {
			return nil, fmt.Errorf("failed to set BootDevice, ok: %v, err: %w", ok, err)
		}
		md := bmcClient.GetMetadata()
		logger.Info("boot device set successfully", "providersAttempted", md.ProvidersAttempted, "successfulProvider", md.SuccessfulProvider, "ok", ok)

		return nil, nil
	}

	if task.VirtualMediaAction != nil {
		ok, err := bmcClient.SetVirtualMedia(ctx, string(task.VirtualMediaAction.Kind), task.VirtualMediaAction.MediaURL)
		if err != nil {
			return nil, fmt.Errorf("failed to perform SetVirtualMedia: %w", err)
		}
		md := bmcClient.GetMetadata()
		logger.Info("virtual media set successfully", "providersAttempted", md.ProvidersAttempted, "successfulProvider", md.SuccessfulProvider, "ok", ok)

		return nil, nil
	}

	if task.PowerCapAction != nil {
		var limit *float64
		if !task.PowerCapAction.Disable && task.PowerCapAction.LimitWatts != nil {
			w := float64(*task.PowerCapAction.LimitWatts)
			limit = &w
		}
		if err := bmcClient.SetPowerCap(ctx, limit); err != nil {
			if isCapabilityUnsupported(err) {
				return nil, fmt.Errorf("power cap: capability not supported by BMC: %w", err)
			}
			return nil, fmt.Errorf("failed to perform PowerCapAction: %w", err)
		}
		logger.Info("power cap set successfully", "disable", task.PowerCapAction.Disable)

		return nil, nil
	}

	if task.SecureBootAction != nil {
		if err := bmcClient.SetSecureBoot(ctx, task.SecureBootAction.Enable); err != nil {
			if isCapabilityUnsupported(err) {
				return nil, fmt.Errorf("secure boot: capability not supported by BMC: %w", err)
			}
			return nil, fmt.Errorf("failed to perform SecureBootAction: %w", err)
		}
		result := map[string]string{"secureBootRequested": strconv.FormatBool(task.SecureBootAction.Enable)}
		if state, err := bmcClient.GetSecureBoot(ctx); err == nil {
			result["secureBootEnabled"] = strconv.FormatBool(state.Enabled)
			result["secureBootMode"] = state.Mode
		}
		logger.Info("secure boot set successfully", "enable", task.SecureBootAction.Enable)

		return result, nil
	}

	if task.InventoryAction != nil {
		device, err := bmcClient.Inventory(ctx)
		if err != nil {
			if isCapabilityUnsupported(err) {
				return nil, fmt.Errorf("inventory: capability not supported by BMC: %w", err)
			}
			return nil, fmt.Errorf("failed to perform InventoryAction: %w", err)
		}
		result := map[string]string{
			"vendor": device.Vendor,
			"model":  device.Model,
			"cpus":   strconv.Itoa(len(device.CPUs)),
			"memory": strconv.Itoa(len(device.Memory)),
			"drives": strconv.Itoa(len(device.Drives)),
			"nics":   strconv.Itoa(len(device.NICs)),
		}
		logger.Info("inventory read successfully", "vendor", device.Vendor, "model", device.Model)

		return result, nil
	}

	if task.FirmwareAction != nil {
		return r.runFirmwareAction(ctx, logger, task.FirmwareAction, bmcClient)
	}

	logger.Info("no action specified in Task, nothing to do", "task", task)

	return nil, errors.New("no action specified in Task, nothing to do")
}

// runFirmwareAction fetches the image, initiates the install via bmclib, and
// polls the resulting task to a terminal state. The provider owns the XCC push
// protocol (claim/push/poll/release) and never GETs the TaskMonitor URI; rufio
// only polls through bmclib's FirmwareInstallStatus.
func (r *TaskReconciler) runFirmwareAction(ctx context.Context, logger logr.Logger, fa *bmc.FirmwareAction, bmcClient *bmclib.Client) (map[string]string, error) {
	rc, err := r.fetchImage(ctx, fa.ImageURL)
	if err != nil {
		return nil, fmt.Errorf("firmware: fetching image %q: %w", fa.ImageURL, err)
	}
	defer rc.Close()

	taskID, err := bmcClient.FirmwareInstall(ctx, fa.Component, fa.ApplyTime, fa.Force, rc)
	if err != nil {
		if isCapabilityUnsupported(err) {
			return nil, fmt.Errorf("firmware install: capability not supported by BMC: %w", err)
		}
		return nil, fmt.Errorf("failed to initiate FirmwareInstall: %w", err)
	}
	result := map[string]string{"firmwareTaskID": taskID, "firmwareComponent": fa.Component}
	logger.Info("firmware install initiated", "taskID", taskID, "component", fa.Component)

	// Poll the install task to a terminal state. Inline + bounded: a Task is a
	// one-shot action, and the provider abstracts the XCC claim/push/release.
	pollCtx, cancel := context.WithTimeout(ctx, firmwarePollTimeout)
	defer cancel()
	ticker := time.NewTicker(firmwarePollInterval)
	defer ticker.Stop()
	for {
		status, serr := bmcClient.FirmwareInstallStatus(pollCtx, "", fa.Component, taskID)
		if serr == nil {
			result["firmwareState"] = status
			switch status {
			case constants.FirmwareInstallComplete:
				logger.Info("firmware install complete", "taskID", taskID)
				return result, nil
			case constants.FirmwareInstallFailed:
				return result, fmt.Errorf("firmware install task %s failed", taskID)
			}
		} else {
			logger.Info("firmware status poll error (will retry)", "taskID", taskID, "error", serr.Error())
		}

		select {
		case <-pollCtx.Done():
			return result, fmt.Errorf("firmware install task %s did not reach a terminal state: %w", taskID, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// checkTaskStatus checks if Task action completed.
// This is currently limited only to a few PowerAction types.
func (r *TaskReconciler) checkTaskStatus(ctx context.Context, log logr.Logger, task bmc.Action, bmcClient *bmclib.Client) (ctrl.Result, error) {
	// TODO(pokearu): Extend to all actions.
	if task.PowerAction != nil {
		rawState, err := bmcClient.GetPowerState(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get power state: %w", err)
		}
		log = log.WithValues("currentPowerState", rawState)
		log.Info("power state check")

		state := toPowerState(rawState)

		switch *task.PowerAction { //nolint:exhaustive // we only support a few power actions right now.
		case bmc.PowerOn:
			if state != bmc.On {
				log.Info("requeuing task", "requeueAfter", powerActionRequeueAfter)
				return ctrl.Result{RequeueAfter: powerActionRequeueAfter}, nil
			}
		case bmc.PowerHardOff, bmc.PowerSoftOff:
			if bmc.Off != state {
				return ctrl.Result{RequeueAfter: powerActionRequeueAfter}, nil
			}
		}
	}

	// Other Task action types do not support checking status. So noop.
	return ctrl.Result{}, nil
}

// mergeResult merges action output key/value pairs into the Task status Result map.
func mergeResult(task *bmc.Task, result map[string]string) {
	if len(result) == 0 {
		return
	}
	if task.Status.Result == nil {
		task.Status.Result = make(map[string]string, len(result))
	}
	for k, v := range result {
		task.Status.Result[k] = v
	}
}

// patchStatus patches the specified patch on the Task.
func (r *TaskReconciler) patchStatus(ctx context.Context, task *bmc.Task, patch client.Patch) error {
	err := r.client.Status().Patch(ctx, task, patch)
	if err != nil {
		return fmt.Errorf("failed to patch Task %s/%s status: %w", task.Namespace, task.Name, err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager, opts ctrlcontroller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		For(&bmc.Task{}).
		Complete(r)
}
