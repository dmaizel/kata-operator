/*


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

package controllers

import (
	"bytes"
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"text/template"
	"time"

	ignTypes "github.com/coreos/ignition/config/v2_2/types"
	"github.com/go-logr/logr"
	kataconfigurationv1 "github.com/openshift/kata-operator/api/v1"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodeapi "k8s.io/api/node/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// blank assignment to verify that KataConfigOpenShiftReconciler implements reconcile.Reconciler
// var _ reconcile.Reconciler = &KataConfigOpenShiftReconciler{}

// KataConfigOpenShiftReconciler reconciles a KataConfig object
type KataConfigOpenShiftReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	clientset  kubernetes.Interface
	kataConfig *kataconfigurationv1.KataConfig
}

// +kubebuilder:rbac:groups=kataconfiguration.openshift.io,resources=kataconfigs;kataconfigs/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kataconfiguration.openshift.io,resources=kataconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;replicasets;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets/finalizers,resourceNames=manager-role,verbs=update
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get
// +kubebuilder:rbac:groups="";machineconfiguration.openshift.io,resources=nodes;machineconfigs;machineconfigpools;pods;services;services/finalizers;endpoints;persistentvolumeclaims;events;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete

func (r *KataConfigOpenShiftReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("kataconfig", req.NamespacedName)
	r.Log.Info("Reconciling KataConfig in OpenShift Cluster")

	// Fetch the KataConfig instance
	r.kataConfig = &kataconfigurationv1.KataConfig{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, r.kataConfig)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after ctrl request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	return func() (ctrl.Result, error) {
		oldest, err := r.isOldestCR()
		if !oldest && err != nil {
			return reconcile.Result{Requeue: true}, err
		} else if !oldest && err == nil {
			return reconcile.Result{}, nil
		}

		// Check if the KataConfig instance is marked to be deleted, which is
		// indicated by the deletion timestamp being set.
		if r.kataConfig.GetDeletionTimestamp() != nil {
			return r.processKataConfigDeleteRequest()
		}

		// if we are using openshift then make sure that MCO related things are
		// handled only after kata binaries are installed on the nodes
		if r.kataConfig.Status.TotalNodesCount > 0 &&
			len(r.kataConfig.Status.InstallationStatus.InProgress.BinariesInstalledNodesList) == r.kataConfig.Status.TotalNodesCount {
			return r.monitorKataConfigInstallation()
		}

		// Once all the nodes have installed kata binaries and configured the CRI runtime create the runtime class
		if r.kataConfig.Status.TotalNodesCount > 0 &&
			r.kataConfig.Status.InstallationStatus.Completed.CompletedNodesCount == r.kataConfig.Status.TotalNodesCount &&
			r.kataConfig.Status.RuntimeClass == "" {

			err := r.deleteKataDaemonset(InstallOperation)
			if err != nil {
				return ctrl.Result{}, err
			}

			return r.setRuntimeClass()
		}
		// Intiate the installation of kata runtime on the nodes if it doesn't exist already
		return r.processKataConfigInstallRequest()
	}()
}

func (r *KataConfigOpenShiftReconciler) processDaemonsetForCR(operation DaemonOperation) *appsv1.DaemonSet {
	var (
		runPrivileged           = true
		configmapOptional       = true
		runAsUser         int64 = 0
	)

	dsName := "kata-operator-daemon-" + string(operation)
	labels := map[string]string{
		"name": dsName,
	}

	var nodeSelector map[string]string
	if r.kataConfig.Spec.KataConfigPoolSelector != nil {
		nodeSelector = r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels
	} else {
		nodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
		}
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: "kata-operator-system",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "default",
					NodeSelector:       nodeSelector,
					Containers: []corev1.Container{
						{
							Name:            "kata-install-pod",
							Image:           "quay.io/isolatedcontainers/kata-operator-daemon@sha256:528c7f6b9495f4ac13c156f79f59023b46b1817250f51ac88c73fd4163d45f8f",
							ImagePullPolicy: "Always",
							SecurityContext: &corev1.SecurityContext{
								Privileged: &runPrivileged,
								RunAsUser:  &runAsUser,
							},
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.Handler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", "rm -rf /host/opt/kata-install /host/usr/local/kata/"},
									},
								},
							},
							Command: []string{"/bin/sh", "-c", fmt.Sprintf("/daemon --resource %s --operation %s", r.kataConfig.Name, operation)},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "hostroot",
									MountPath: "/host",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "KATA_PAYLOAD_IMAGE",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "payload-config",
											},
											Key:      "daemon.payload",
											Optional: &configmapOptional,
										},
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "hostroot", // Has to match VolumeMounts in containers
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/",
									//Type: &corev1.HostPathVolumeSource,
								},
							},
						},
					},
					HostNetwork: true,
					HostPID:     true,
				},
			},
		},
	}
}

func (r *KataConfigOpenShiftReconciler) newMCPforCR() *mcfgv1.MachineConfigPool {
	lsr := metav1.LabelSelectorRequirement{
		Key:      "machineconfiguration.openshift.io/role",
		Operator: metav1.LabelSelectorOpIn,
		Values:   []string{"kata-oc", "worker"},
	}

	var nodeSelector *metav1.LabelSelector

	if r.kataConfig.Spec.KataConfigPoolSelector != nil {
		nodeSelector = r.kataConfig.Spec.KataConfigPoolSelector
	}

	mcp := &mcfgv1.MachineConfigPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "machineconfiguration.openshift.io/v1",
			Kind:       "MachineConfigPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "kata-oc",
		},
		Spec: mcfgv1.MachineConfigPoolSpec{
			MachineConfigSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{lsr},
			},
			NodeSelector: nodeSelector,
		},
	}

	return mcp
}

func (r *KataConfigOpenShiftReconciler) newMCForCR(machinePool string) (*mcfgv1.MachineConfig, error) {
	isenabled := true
	name := "kata-osbuilder-generate.service"
	content := `
[Unit]
Description=Hacky service to enable kata-osbuilder-generate.service
ConditionPathExists=/usr/lib/systemd/system/kata-osbuilder-generate.service
[Service]
Type=oneshot
ExecStart=/usr/libexec/kata-containers/osbuilder/kata-osbuilder.sh
ExecRestart=/usr/libexec/kata-containers/osbuilder/kata-osbuilder.sh
[Install]
WantedBy=multi-user.target
`

	kataOC, err := r.kataOcExists()
	if err != nil {
		return nil, err
	}

	if kataOC {
		machinePool = "kata-oc"
	} else if _, ok := r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels["node-role.kubernetes.io/"+machinePool]; ok {
		r.Log.Info("in newMCforCR machinePool" + machinePool)
	} else {
		r.Log.Error(err, "no valid role for mc found")
	}

	file := ignTypes.File{}
	c := ignTypes.FileContents{}

	dropinConf, err := generateDropinConfig(r.kataConfig.Status.RuntimeClass)
	if err != nil {
		return nil, err
	}

	c.Source = "data:text/plain;charset=utf-8;base64," + dropinConf
	file.Contents = c
	file.Filesystem = "root"
	m := 420
	file.Mode = &m
	file.Path = "/etc/crio/crio.conf.d/50-kata.conf"

	ic := ignTypes.Config{
		Ignition: ignTypes.Ignition{
			Version: "2.2.0",
		},
		Systemd: ignTypes.Systemd{
			Units: []ignTypes.Unit{
				{Name: name, Enabled: &isenabled, Contents: content},
			},
		},
	}
	ic.Storage.Files = []ignTypes.File{file}

	icb, err := json.Marshal(ic)
	if err != nil {
		return nil, err
	}

	mc := *&mcfgv1.MachineConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "machineconfiguration.openshift.io/v1",
			Kind:       "MachineConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "50-kata-crio-dropin",
			Labels: map[string]string{
				"machineconfiguration.openshift.io/role": machinePool,
				"app":                                    r.kataConfig.Name,
			},
			Namespace: "kata-operator",
		},
		Spec: mcfgv1.MachineConfigSpec{
			Config: runtime.RawExtension{
				Raw: icb,
			},
		},
	}

	return &mc, nil
}

func generateDropinConfig(handlerName string) (string, error) {
	var err error
	buf := new(bytes.Buffer)
	type RuntimeConfig struct {
		RuntimeName string
	}
	const b = `
[crio.runtime]
  manage_ns_lifecycle = true

[crio.runtime.runtimes.{{.RuntimeName}}]
  runtime_path = "/usr/bin/containerd-shim-kata-v2"
  runtime_type = "vm"
  runtime_root = "/run/vc"
  
[crio.runtime.runtimes.runc]
  runtime_path = ""
  runtime_type = "oci"
  runtime_root = "/run/runc"
`
	c := RuntimeConfig{RuntimeName: "kata"}
	t := template.Must(template.New("test").Parse(b))
	err = t.Execute(buf, c)
	if err != nil {
		return "", err
	}
	sEnc := b64.StdEncoding.EncodeToString([]byte(buf.String()))
	return sEnc, err
}

func (r *KataConfigOpenShiftReconciler) addFinalizer() error {
	r.Log.Info("Adding Finalizer for the KataConfig")
	controllerutil.AddFinalizer(r.kataConfig, kataConfigFinalizer)

	// Update CR
	err := r.Client.Update(context.TODO(), r.kataConfig)
	if err != nil {
		r.Log.Error(err, "Failed to update KataConfig with finalizer")
		return err
	}
	return nil
}

func (r *KataConfigOpenShiftReconciler) listKataPods() error {
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(corev1.NamespaceAll),
	}
	if err := r.Client.List(context.TODO(), podList, listOpts...); err != nil {
		return fmt.Errorf("Failed to list kata pods: %v", err)
	}
	for _, pod := range podList.Items {
		if pod.Spec.RuntimeClassName != nil {
			if *pod.Spec.RuntimeClassName == r.kataConfig.Status.RuntimeClass {
				return fmt.Errorf("Existing pods using Kata Runtime found. Please delete the pods manually for KataConfig deletion to proceed")
			}
		}
	}
	return nil
}

func (r *KataConfigOpenShiftReconciler) kataOcExists() (bool, error) {
	kataOcMcp := &mcfgv1.MachineConfigPool{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "kata-oc"}, kataOcMcp)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("No kata-oc machine config pool found!")
		return false, nil
	} else if err != nil {
		r.Log.Error(err, "Could not get the kata-oc machine config pool!")
		return false, err
	}

	return true, nil
}

func (r *KataConfigOpenShiftReconciler) workerOrMaster() (string, error) {
	var role string
	workerMcp := &mcfgv1.MachineConfigPool{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: "worker"}, workerMcp)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Error(err, "No worker machine config pool found!")
		return "", err
	} else if err != nil {
		r.Log.Error(err, "Could not get the worker machine config pool!")
		return "", err
	}

	if workerMcp.Status.MachineCount > 0 {
		role = "worker"
	} else {
		role = "master"
	}
	return role, nil
}

func (r *KataConfigOpenShiftReconciler) processKataConfigInstallRequest() (ctrl.Result, error) {
	if r.kataConfig.Status.TotalNodesCount == 0 {

		nodesList := &corev1.NodeList{}

		/* This could be the case in a compact cluster where master and workers are on the same node */
		machinePool, err := r.workerOrMaster()
		if err != nil {
			return reconcile.Result{}, err
		}

		if r.kataConfig.Spec.KataConfigPoolSelector == nil {
			r.kataConfig.Spec.KataConfigPoolSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"node-role.kubernetes.io/" + machinePool: ""},
			}
		}

		listOpts := []client.ListOption{
			client.MatchingLabels(r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels),
		}

		err = r.Client.List(context.TODO(), nodesList, listOpts...)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.kataConfig.Status.TotalNodesCount = len(nodesList.Items)

		if r.kataConfig.Status.TotalNodesCount == 0 {
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second},
				fmt.Errorf("No suitable worker nodes found for kata installation. Please make sure to label the nodes with labels specified in KataConfigPoolSelector")
		}

		err = r.Client.Status().Update(context.TODO(), r.kataConfig)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if r.kataConfig.Status.KataImage == "" {
		// TODO - placeholder. This will change in future.
		r.kataConfig.Status.KataImage = "quay.io/kata-operator/kata-artifacts:1.0"
	}

	// Don't create the daemonset if kata is already installed on the cluster nodes
	if r.kataConfig.Status.TotalNodesCount > 0 &&
		r.kataConfig.Status.InstallationStatus.Completed.CompletedNodesCount != r.kataConfig.Status.TotalNodesCount {
		ds := r.processDaemonsetForCR(InstallOperation)
		// Set KataConfig instance as the owner and controller
		if err := controllerutil.SetControllerReference(r.kataConfig, ds, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		foundDs := &appsv1.DaemonSet{}
		err := r.Client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, foundDs)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new installation Daemonset", "ds.Namespace", ds.Namespace, "ds.Name", ds.Name)
			err = r.Client.Create(context.TODO(), ds)
			if err != nil {
				return ctrl.Result{}, err
			}
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Add finalizer for this CR
	if !contains(r.kataConfig.GetFinalizers(), kataConfigFinalizer) {
		if err := r.addFinalizer(); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *KataConfigOpenShiftReconciler) setRuntimeClass() (ctrl.Result, error) {
	runtimeClassName := "kata"

	rc := func() *nodeapi.RuntimeClass {
		rc := &nodeapi.RuntimeClass{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "node.k8s.io/v1beta1",
				Kind:       "RuntimeClass",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: runtimeClassName,
			},
			Handler: runtimeClassName,
			// Use same values for Pod Overhead as upstream kata-deploy using, see
			// https://github.com/kata-containers/packaging/blob/f17450317563b6e4d6b1a71f0559360b37783e19/kata-deploy/k8s-1.18/kata-runtimeClasses.yaml#L7
			Overhead: &nodeapi.Overhead{
				PodFixed: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("160Mi"),
				},
			},
		}

		if r.kataConfig.Spec.KataConfigPoolSelector != nil {
			rc.Scheduling = &nodeapi.Scheduling{
				NodeSelector: r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels,
			}
		}
		return rc
	}()

	// Set Kataconfig r.kataConfig as the owner and controller
	if err := controllerutil.SetControllerReference(r.kataConfig, rc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	foundRc := &nodeapi.RuntimeClass{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: rc.Name}, foundRc)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new RuntimeClass", "rc.Name", rc.Name)
		err = r.Client.Create(context.TODO(), rc)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if r.kataConfig.Status.RuntimeClass == "" {
		r.kataConfig.Status.RuntimeClass = runtimeClassName
		err = r.Client.Status().Update(context.TODO(), r.kataConfig)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *KataConfigOpenShiftReconciler) processKataConfigDeleteRequest() (ctrl.Result, error) {
	r.Log.Info("KataConfig deletion in progress: ")
	machinePool, err := r.workerOrMaster()
	if err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
	}

	if contains(r.kataConfig.GetFinalizers(), kataConfigFinalizer) {
		// Get the list of pods that might be running using kata runtime
		err := r.listKataPods()
		if err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, err
		}

		ds := r.processDaemonsetForCR(UninstallOperation)

		foundDs := &appsv1.DaemonSet{}
		err = r.Client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, foundDs)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new uninstallation Daemonset", "ds.Namespace", ds.Namespace, "ds.Name", ds.Name)
			err = r.Client.Create(context.TODO(), ds)
			if err != nil {
				return ctrl.Result{}, err
			}
		} else if err != nil {
			return ctrl.Result{}, err
		}

		if r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesCount != r.kataConfig.Status.TotalNodesCount {
			r.Log.Info("KataConfig uninstallation: ", "Number of nodes completed uninstallation ",
				r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesCount,
				"Total number of kata installed nodes ", r.kataConfig.Status.TotalNodesCount)
			// TODO - we don't need this nil check if we know that pool is always initialized
			if r.kataConfig.Spec.KataConfigPoolSelector != nil &&
				r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels != nil && len(r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels) > 0 {
				if r.clientset == nil {
					r.clientset, err = getClientSet()
					if err != nil {
						return ctrl.Result{}, err
					}
				}

				for _, nodeName := range r.kataConfig.Status.UnInstallationStatus.InProgress.BinariesUnInstalledNodesList {
					if contains(r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesList, nodeName) {
						continue
					}

					if _, ok := r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels["node-role.kubernetes.io/"+machinePool]; !ok {
						r.Log.Info("Removing the kata pool selector label from the node", "node name ", nodeName)
						node, err := r.clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
						if err != nil {
							return ctrl.Result{}, err
						}

						nodeLabels := node.GetLabels()

						for k := range r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels {
							delete(nodeLabels, k)
						}

						node.SetLabels(nodeLabels)
						_, err = r.clientset.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})

						if err != nil {
							return ctrl.Result{}, err
						}
					}
				}
			}
		}

		r.Log.Info("Making sure parent MCP is synced properly, KataNodeRole=" + machinePool)
		if _, ok := r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels["node-role.kubernetes.io/"+machinePool]; ok {
			mc, err := r.newMCForCR(machinePool)
			var isMcDeleted bool

			err = r.Client.Get(context.TODO(), types.NamespacedName{Name: mc.Name}, mc)
			if err != nil && errors.IsNotFound(err) {
				isMcDeleted = true
			} else if err != nil {
				return ctrl.Result{}, err
			}

			if !isMcDeleted {
				err = r.Client.Delete(context.TODO(), mc)
				if err != nil {
					// error during removing mc, don't block the uninstall. Just log the error and move on.
					r.Log.Info("Error found deleting machine config. If the machine config exists after installation it can be safely deleted manually.",
						"mc", mc.Name, "error", err)
				}
				// Sleep for MCP to reflect the changes
				r.Log.Info("Pausing for a minute to make sure worker mcp has started syncing up")
				time.Sleep(60 * time.Second)
			}

			workreMcp := &mcfgv1.MachineConfigPool{}
			err = r.Client.Get(context.TODO(), types.NamespacedName{Name: machinePool}, workreMcp)
			if err != nil {
				return ctrl.Result{}, err
			}
			r.Log.Info("Monitoring worker mcp", "worker mcp name", workreMcp.Name, "ready machines", workreMcp.Status.ReadyMachineCount,
				"total machines", workreMcp.Status.MachineCount)
			if workreMcp.Status.ReadyMachineCount != workreMcp.Status.MachineCount {
				return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
			}
		} else {
			// Sleep for MCP to reflect the changes
			if len(r.kataConfig.Status.UnInstallationStatus.InProgress.BinariesUnInstalledNodesList) > 0 {
				r.Log.Info("Pausing for a minute to make sure parent mcp has started syncing up")
				time.Sleep(60 * time.Second)

				parentMcp := &mcfgv1.MachineConfigPool{}

				err := r.Client.Get(context.TODO(), types.NamespacedName{Name: machinePool}, parentMcp)
				if err != nil && errors.IsNotFound(err) {
					return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, fmt.Errorf("Not able to find parent pool %s", parentMcp.GetName())
				} else if err != nil {
					return ctrl.Result{}, err
				}

				r.Log.Info("Monitoring parent mcp", "parent mcp name", parentMcp.Name, "ready machines", parentMcp.Status.ReadyMachineCount,
					"total machines", parentMcp.Status.MachineCount)
				if parentMcp.Status.ReadyMachineCount != parentMcp.Status.MachineCount {
					return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
				}

				mcp := r.newMCPforCR()
				err = r.Client.Delete(context.TODO(), mcp)
				if err != nil {
					// error during removing mcp, don't block the uninstall. Just log the error and move on.
					r.Log.Info("Error found deleting mcp. If the mcp exists after installation it can be safely deleted manually.",
						"mcp", mcp.Name, "error", err)
				}

				mc, err := r.newMCForCR(machinePool)
				err = r.Client.Delete(context.TODO(), mc)
				if err != nil {
					// error during removing mc, don't block the uninstall. Just log the error and move on.
					r.Log.Info("Error found deleting machine config. If the machine config exists after installation it can be safely deleted manually.",
						"mc", mc.Name, "error", err)
				}
			} else {
				return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
			}
		}

		for _, nodeName := range r.kataConfig.Status.UnInstallationStatus.InProgress.BinariesUnInstalledNodesList {
			if contains(r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesList, nodeName) {
				continue
			}

			r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesCount++
			r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesList = append(r.kataConfig.Status.UnInstallationStatus.Completed.CompletedNodesList, nodeName)
			if r.kataConfig.Status.UnInstallationStatus.InProgress.InProgressNodesCount > 0 {
				r.kataConfig.Status.UnInstallationStatus.InProgress.InProgressNodesCount--
			}
		}

		err = r.Client.Status().Update(context.TODO(), r.kataConfig)
		if err != nil {
			return ctrl.Result{}, err
		}

		r.Log.Info("Deleting uninstall daemonset")
		err = r.deleteKataDaemonset(UninstallOperation)
		if err != nil {
			return ctrl.Result{}, err
		}

		r.Log.Info("Uninstallation completed on all nodes. Proceeding with the KataConfig deletion")
		controllerutil.RemoveFinalizer(r.kataConfig, kataConfigFinalizer)
		err = r.Client.Update(context.TODO(), r.kataConfig)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *KataConfigOpenShiftReconciler) deleteKataDaemonset(operation DaemonOperation) error {

	ds := r.processDaemonsetForCR(operation)
	foundDs := &appsv1.DaemonSet{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, foundDs)
	if err != nil && errors.IsNotFound(err) {
		// DaemonSet not found, nothing to delete, ignore the request.
		return nil
	} else if err != nil {
		return err
	}

	err = r.Client.Delete(context.TODO(), foundDs)
	if err != nil {
		return err
	}

	return nil
}

func (r *KataConfigOpenShiftReconciler) monitorKataConfigInstallation() (ctrl.Result, error) {
	r.Log.Info("installation is complete on targetted nodes, now dropping in crio config using MCO")
	machinePool, err := r.workerOrMaster()
	if err != nil {
		return reconcile.Result{}, err
	}

	if _, ok := r.kataConfig.Spec.KataConfigPoolSelector.MatchLabels["node-role.kubernetes.io/"+machinePool]; !ok {
		r.Log.Info("creating new Mcp")
		mcp := r.newMCPforCR()

		founcMcp := &mcfgv1.MachineConfigPool{}
		err := r.Client.Get(context.TODO(), types.NamespacedName{Name: mcp.Name}, founcMcp)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new Machine Config Pool ", "mcp.Name", mcp.Name)
			err = r.Client.Create(context.TODO(), mcp)
			if err != nil {
				return ctrl.Result{}, err
			}
			// mcp created successfully - requeue to check the status later
			return ctrl.Result{Requeue: true, RequeueAfter: 20 * time.Second}, nil
		} else if err != nil {
			return ctrl.Result{}, err
		}

		// Wait till MCP is ready
		if founcMcp.Status.MachineCount == 0 {
			r.Log.Info("Waiting till Machine Config Pool is initialized ", "mcp.Name", mcp.Name)
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
		}
		if founcMcp.Status.MachineCount != founcMcp.Status.ReadyMachineCount {
			r.Log.Info("Waiting till Machine Config Pool is ready ", "mcp.Name", mcp.Name)
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}, nil
		}
	}

	r.Log.Info("KataNodeRole is: " + machinePool)
	mc, err := r.newMCForCR(machinePool)
	if err != nil {
		return ctrl.Result{}, err
	}

	foundMc := &mcfgv1.MachineConfig{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: mc.Name}, foundMc)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new Machine Config ", "mc.Name", mc.Name)
		err = r.Client.Create(context.TODO(), mc)
		if err != nil {
			return ctrl.Result{}, err
		}
		// mc created successfully - don't requeue
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KataConfigOpenShiftReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kataconfigurationv1.KataConfig{}).
		Complete(r)
}

func (r *KataConfigOpenShiftReconciler) isOldestCR() (bool, error) {
	kataConfigList := &kataconfigurationv1.KataConfigList{}
	listOpts := []client.ListOption{
		client.InNamespace(corev1.NamespaceAll),
	}
	if err := r.Client.List(context.TODO(), kataConfigList, listOpts...); err != nil {
		return false, fmt.Errorf("Failed to list KataConfig custom resources: %v", err)
	}

	if len(kataConfigList.Items) == 1 {
		return true, nil
	}

	// Creation time of the CR of the current reconciliation request
	tkccd := r.kataConfig.GetCreationTimestamp()

	// holds the oldest CR found so far
	var oldestCR *kataconfigurationv1.KataConfig

	for index := range kataConfigList.Items {
		if kataConfigList.Items[index].Name == r.kataConfig.Name {
			continue
		}

		// Creation time of this instance of CR in the loop
		ckccd := kataConfigList.Items[index].GetCreationTimestamp()

		if oldestCR == nil {
			oldestCR = &kataConfigList.Items[index]
		} else {
			oldestCreationDateSoFar := oldestCR.GetCreationTimestamp()
			if !oldestCreationDateSoFar.Before(&ckccd) {
				oldestCR = &kataConfigList.Items[index]
			}
		}
	}

	oldestCRCreationDate := oldestCR.GetCreationTimestamp()
	if !tkccd.Before(&oldestCRCreationDate) {
		if r.kataConfig.Status.InstallationStatus.Failed.FailedNodesCount != -1 {
			r.kataConfig.Status.InstallationStatus.Failed.FailedNodesCount = -1
			r.kataConfig.Status.InstallationStatus.Failed.FailedNodesList = []kataconfigurationv1.FailedNodeStatus{
				{
					Name:  "",
					Error: fmt.Sprintf("Multiple KataConfig CRs are not supported, %s already exists", oldestCR.Name),
				},
			}

			err := r.Client.Status().Update(context.TODO(), r.kataConfig)
			if err != nil {
				return false, err
			}

			return false, nil
		}
	}

	return true, nil
}
