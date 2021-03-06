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
	"context"
	"fmt"
	"github.com/go-logr/logr"
	util "github.com/openstack-k8s-operators/lib-common/pkg/util"
	"github.com/openstack-k8s-operators/neutron-operator/pkg/neutronsriovagent"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"time"

	neutronv1beta1 "github.com/openstack-k8s-operators/neutron-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

// CommonConfigMAP
const (
	CommonConfigMAP string = "common-config"
)

var ospHostAliases = []corev1.HostAlias{}

// NeutronSriovAgentReconciler reconciles a NeutronSriovAgent object
type NeutronSriovAgentReconciler struct {
	Client client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=neutron.openstack.org,resources=neutronsriovagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=neutron.openstack.org,resources=neutronsriovagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;create;update;delete;
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;delete;

// Reconcile reconcile keystone API requests
func (r *NeutronSriovAgentReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("neutronsriovagent", req.NamespacedName)
	r.Log.Info("Reconciling NeutronSriovAgent")

	// Fetch the NeutronSriovAgent instance
	instance := &neutronv1beta1.NeutronSriovAgent{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	commonConfigMap := &corev1.ConfigMap{}

	r.Log.Info("Creating host entries from config map:", "configMap: ", CommonConfigMAP)
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: CommonConfigMAP, Namespace: instance.Namespace}, commonConfigMap)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Error(err, "common-config ConfigMap not found!", "Instance.Namespace", instance.Namespace, "Instance.Name", instance.Name)
		return ctrl.Result{}, err
	}

	if err := controllerutil.SetControllerReference(instance, commonConfigMap, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// Create additional host entries added to the /etc/hosts file of the containers
	ospHostAliases, err = util.CreateOspHostsEntries(commonConfigMap)
	if err != nil {
		r.Log.Error(err, "Failed ospHostAliases", "Instance.Namespace", instance.Namespace, "Instance.Name", instance.Name)
		return ctrl.Result{}, err
	}

	// ConfigMap
	configMap := neutronsriovagent.ConfigMap(instance, instance.Name)
	if err := controllerutil.SetControllerReference(instance, configMap, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Check if this ConfigMap already exists
	foundConfigMap := &corev1.ConfigMap{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: configMap.Name, Namespace: configMap.Namespace}, foundConfigMap)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new ConfigMap", "ConfigMap.Namespace", configMap.Namespace, "Job.Name", configMap.Name)
		err = r.Client.Create(context.TODO(), configMap)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if !reflect.DeepEqual(configMap.Data, foundConfigMap.Data) {
		r.Log.Info("Updating ConfigMap")

		configMap.Data = foundConfigMap.Data
	}

	configMapHash, err := util.ObjectHash(configMap)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error calculating configuration hash: %v", err)
	}
	r.Log.Info("ConfigMapHash: ", "Data Hash:", configMapHash)

	// Define a new Daemonset object
	ds := newDaemonset(instance, instance.Name, configMapHash)
	dsHash, err := util.ObjectHash(ds)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error calculating configuration hash: %v", err)
	}
	r.Log.Info("DaemonsetHash: ", "Daemonset Hash:", dsHash)

	// Set NeutronSriovAgent instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, ds, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// Check if this Daemonset already exists
	found := &appsv1.DaemonSet{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new Daemonset", "ds.Namespace", ds.Namespace, "ds.Name", ds.Name)
		err = r.Client.Create(context.TODO(), ds)
		if err != nil {
			return ctrl.Result{}, err
		}

		// ds created successfully - don't requeue
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	} else {

		if instance.Status.DaemonsetHash != dsHash {
			r.Log.Info("Daemonset Updated")
			found.Spec = ds.Spec
			err = r.Client.Update(context.TODO(), found)
			if err != nil {
				return ctrl.Result{}, err
			}
			r.setDaemonsetHash(instance, dsHash)
			return ctrl.Result{RequeueAfter: time.Second * 10}, err
		}
	}

	// Daemonset already exists - don't requeue
	r.Log.Info("Skip reconcile: Daemonset already exists", "ds.Namespace", found.Namespace, "ds.Name", found.Name)
	return ctrl.Result{}, nil
}

func (r *NeutronSriovAgentReconciler) setDaemonsetHash(instance *neutronv1beta1.NeutronSriovAgent, hashStr string) error {

	if hashStr != instance.Status.DaemonsetHash {
		instance.Status.DaemonsetHash = hashStr
		if err := r.Client.Status().Update(context.TODO(), instance); err != nil {
			return err
		}
	}
	return nil

}

func newDaemonset(cr *neutronv1beta1.NeutronSriovAgent, cmName string, configHash string) *appsv1.DaemonSet {
	var bidirectional = corev1.MountPropagationBidirectional
	var hostToContainer = corev1.MountPropagationHostToContainer
	var trueVar = true
	var configVolumeDefaultMode int32 = 0644
	var dirOrCreate = corev1.HostPathDirectoryOrCreate

	daemonSet := appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: cr.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"daemonset": cr.Name + "-daemonset"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"daemonset": cr.Name + "-daemonset"},
				},
				Spec: corev1.PodSpec{
					NodeSelector:   map[string]string{"daemon": cr.Spec.Label},
					HostNetwork:    true,
					HostPID:        true,
					DNSPolicy:      "ClusterFirstWithHostNet",
					HostAliases:    ospHostAliases,
					InitContainers: []corev1.Container{},
					Containers:     []corev1.Container{},
				},
			},
		},
	}

	initContainerSpec := corev1.Container{
		Name:  "sriov-agent-config-init",
		Image: cr.Spec.NeutronSriovImage,
		SecurityContext: &corev1.SecurityContext{
			Privileged: &trueVar,
		},
		Command: []string{
			"/bin/bash", "-c", "export CTRL_IP_TENANT=$(getent hosts controller-0.tenant | awk '{print $1}') && export POD_IP_TENANT=$(ip route get $CTRL_IP_TENANT | awk '{print $5}') && cp -a /etc/neutron/* /tmp/neutron/",
		},
		Env: []corev1.EnvVar{
			{
				Name: "MY_POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "status.podIP",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      cmName,
				ReadOnly:  true,
				MountPath: "/etc/neutron/neutron.conf",
				SubPath:   "neutron.conf",
			},
			{
				Name:      cmName,
				ReadOnly:  true,
				MountPath: "/etc/neutron/plugins/ml2/sriov_agent.ini",
				SubPath:   "sriov_agent.ini",
			},
			{
				Name:      "etc-machine-id",
				MountPath: "/etc/machine-id",
				ReadOnly:  true,
			},
			{
				Name:      "neutron-config-vol",
				MountPath: "/tmp/neutron",
				ReadOnly:  false,
			},
		},
	}
	daemonSet.Spec.Template.Spec.InitContainers = append(daemonSet.Spec.Template.Spec.InitContainers, initContainerSpec)

	neutronSriovAgentContainerSpec := corev1.Container{
		Name:  "neutron-sriov-agent",
		Image: cr.Spec.NeutronSriovImage,
		Command: []string{
			"/bin/sleep", "86400",
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: &trueVar,
		},
		Env: []corev1.EnvVar{
			{
				Name:  "CONFIG_HASH",
				Value: configHash,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      cmName,
				ReadOnly:  true,
				MountPath: "/etc/neutron/neutron.conf",
				SubPath:   "neutron.conf",
			},
			{
				Name:      cmName,
				ReadOnly:  true,
				MountPath: "/etc/neutron/plugins/ml2/openvswitch_agent.ini",
				SubPath:   "openvswitch_agent.ini",
			},
			{
				Name:      "etc-machine-id",
				MountPath: "/etc/machine-id",
				ReadOnly:  true,
			},
			{
				Name:             "lib-modules-volume",
				MountPath:        "/lib/modules",
				MountPropagation: &hostToContainer,
			},
			{
				Name:             "run-openvswitch-volume",
				MountPath:        "/var/run/openvswitch",
				MountPropagation: &bidirectional,
			},
			{
				Name:             "neutron-log-volume",
				MountPath:        "/var/log/neutron",
				MountPropagation: &bidirectional,
			},
			{
				Name:      "neutron-config-vol",
				MountPath: "/etc/nova",
				ReadOnly:  false,
			},
		},
	}
	daemonSet.Spec.Template.Spec.Containers = append(daemonSet.Spec.Template.Spec.Containers, neutronSriovAgentContainerSpec)

	volConfigs := []corev1.Volume{
		{
			Name: "etc-machine-id",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/machine-id",
				},
			},
		},
		{
			Name: "run-volume",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/run",
				},
			},
		},
		{
			Name: "lib-modules-volume",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/lib/modules",
				},
			},
		},
		{
			Name: "run-openvswitch-volume",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/openvswitch",
					Type: &dirOrCreate,
				},
			},
		},
		{
			Name: "neutron-log-volume",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/log/containers/neutron",
					Type: &dirOrCreate,
				},
			},
		},
		{
			Name: cmName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					DefaultMode: &configVolumeDefaultMode,
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cmName,
					},
				},
			},
		},
		{
			Name: "neutron-config-vol",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}
	for _, volConfig := range volConfigs {
		daemonSet.Spec.Template.Spec.Volumes = append(daemonSet.Spec.Template.Spec.Volumes, volConfig)
	}

	return &daemonSet
}

// SetupWithManager x
func (r *NeutronSriovAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&neutronv1beta1.NeutronSriovAgent{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
