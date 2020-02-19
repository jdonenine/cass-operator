package reconciliation

// This file defines constructors for k8s objects

import (
	"fmt"

	datastaxv1alpha1 "github.com/riptano/dse-operator/operator/pkg/apis/datastax/v1alpha1"
	"github.com/riptano/dse-operator/operator/pkg/dsereconciliation"
	"github.com/riptano/dse-operator/operator/pkg/httphelper"
	"github.com/riptano/dse-operator/operator/pkg/oplabels"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Creates a headless service object for the DSE Datacenter, for clients wanting to
// reach out to a ready DSE node for either CQL or mgmt API
func newServiceForDseDatacenter(dseDatacenter *datastaxv1alpha1.DseDatacenter) *corev1.Service {
	svcName := dseDatacenter.GetDseDatacenterServiceName()
	service := makeGenericHeadlessService(dseDatacenter)
	service.ObjectMeta.Name = svcName
	service.Spec.Ports = []corev1.ServicePort{
		// Note: Port Names cannot be more than 15 characters
		{
			Name: "native", Port: 9042, TargetPort: intstr.FromInt(9042),
		},
		{
			Name: "mgmt-api", Port: 8080, TargetPort: intstr.FromInt(8080),
		},
	}
	return service
}

func buildLabelSelectorForSeedService(dseDatacenter *datastaxv1alpha1.DseDatacenter) map[string]string {
	labels := dseDatacenter.GetClusterLabels()

	// narrow selection to just the seed nodes
	labels[datastaxv1alpha1.SeedNodeLabel] = "true"

	return labels
}

// newSeedServiceForDseDatacenter creates a headless service owned by the DseDatacenter which will attach to all seed
// nodes in the cluster
func newSeedServiceForDseDatacenter(dseDatacenter *datastaxv1alpha1.DseDatacenter) *corev1.Service {
	service := makeGenericHeadlessService(dseDatacenter)
	service.ObjectMeta.Name = dseDatacenter.GetSeedServiceName()

	labels := dseDatacenter.GetClusterLabels()
	oplabels.AddManagedByLabel(labels)
	service.ObjectMeta.Labels = labels

	service.Spec.Selector = buildLabelSelectorForSeedService(dseDatacenter)
	service.Spec.PublishNotReadyAddresses = true
	return service
}

// newAllDsePodsServiceForDseDatacenter creates a headless service owned by the DseDatacenter,
// which covers all DSE pods in the datacenter, whether they are ready or not
func newAllDsePodsServiceForDseDatacenter(dseDatacenter *datastaxv1alpha1.DseDatacenter) *corev1.Service {
	service := makeGenericHeadlessService(dseDatacenter)
	service.ObjectMeta.Name = dseDatacenter.GetAllPodsServiceName()
	service.Spec.PublishNotReadyAddresses = true
	return service
}

// makeGenericHeadlessService returns a fresh k8s headless (aka ClusterIP equals "None") Service
// struct that has the same namespace as the DseDatacenter argument, and proper labels for the DC.
// The caller needs to fill in the ObjectMeta.Name value, at a minimum, before it can be created
// inside the k8s cluster.
func makeGenericHeadlessService(dseDatacenter *datastaxv1alpha1.DseDatacenter) *corev1.Service {
	labels := dseDatacenter.GetDatacenterLabels()
	oplabels.AddManagedByLabel(labels)
	var service corev1.Service
	service.ObjectMeta.Namespace = dseDatacenter.Namespace
	service.ObjectMeta.Labels = labels
	service.Spec.Selector = labels
	service.Spec.Type = "ClusterIP"
	service.Spec.ClusterIP = "None"
	return &service
}

func newNamespacedNameForStatefulSet(
	dseDc *datastaxv1alpha1.DseDatacenter,
	rackName string) types.NamespacedName {

	name := dseDc.Spec.DseClusterName + "-" + dseDc.Name + "-" + rackName + "-sts"
	ns := dseDc.Namespace

	return types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}
}

// Create a statefulset object for the DSE Datacenter.
func newStatefulSetForDseDatacenter(
	rackName string,
	dseDatacenter *datastaxv1alpha1.DseDatacenter,
	replicaCount int) (*appsv1.StatefulSet, error) {

	replicaCountInt32 := int32(replicaCount)

	podLabels := dseDatacenter.GetRackLabels(rackName)
	oplabels.AddManagedByLabel(podLabels)
	podLabels[datastaxv1alpha1.DseNodeState] = "Ready-to-Start"

	// see https://github.com/kubernetes/kubernetes/pull/74941
	// pvc labels are ignored before k8s 1.15.0
	pvcLabels := dseDatacenter.GetRackLabels(rackName)
	oplabels.AddManagedByLabel(pvcLabels)

	statefulSetLabels := dseDatacenter.GetRackLabels(rackName)
	oplabels.AddManagedByLabel(statefulSetLabels)

	statefulSetSelectorLabels := dseDatacenter.GetRackLabels(rackName)

	dseVersion := dseDatacenter.Spec.DseVersion
	var userID int64 = 999
	var volumeCaimTemplates []corev1.PersistentVolumeClaim
	var dseVolumeMounts []corev1.VolumeMount
	initContainerImage := dseDatacenter.GetConfigBuilderImage()

	racks := dseDatacenter.Spec.GetRacks()
	var zone string
	for _, rack := range racks {
		if rack.Name == rackName {
			zone = rack.Zone
		}
	}

	dseConfigVolumeMount := corev1.VolumeMount{
		Name:      "dse-config",
		MountPath: "/config",
	}

	dseVolumeMounts = append(dseVolumeMounts, dseConfigVolumeMount)

	dseVolumeMounts = append(dseVolumeMounts,
		corev1.VolumeMount{
			Name:      "dse-logs",
			MountPath: "/var/log/cassandra",
		})

	configData, err := dseDatacenter.GetConfigAsJSON()
	if err != nil {
		return nil, err
	}

	// Add storage if storage claim defined
	if nil != dseDatacenter.Spec.StorageClaim {
		pvcName := "dse-data"
		storageClaim := dseDatacenter.Spec.StorageClaim
		dseVolumeMounts = append(dseVolumeMounts, corev1.VolumeMount{
			Name:      pvcName,
			MountPath: "/var/lib/cassandra",
		})
		volumeCaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{
				Labels: pvcLabels,
				Name:   pvcName,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources:        storageClaim.Resources,
				StorageClassName: &storageClaim.StorageClassName,
			},
		}}
	}

	ports, err := dseDatacenter.GetContainerPorts()
	if err != nil {
		return nil, err
	}
	dseImage, err := dseDatacenter.GetServerImage()
	if err != nil {
		return nil, err
	}

	serviceAccount := "default"
	if dseDatacenter.Spec.ServiceAccount != "" {
		serviceAccount = dseDatacenter.Spec.ServiceAccount
	}

	nsName := newNamespacedNameForStatefulSet(dseDatacenter, rackName)

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: podLabels,
		},
		Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{
				NodeAffinity:    calculateNodeAffinity(zone),
				PodAntiAffinity: calculatePodAntiAffinity(dseDatacenter.Spec.AllowMultipleNodesPerWorker),
			},
			// workaround for https://cloud.google.com/kubernetes-engine/docs/security-bulletins#may-31-2019
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &userID,
				RunAsGroup: &userID,
				FSGroup:    &userID,
			},
			Volumes: []corev1.Volume{
				{
					Name: "dse-config",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "dse-logs",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			InitContainers: []corev1.Container{{
				Name:  "dse-config-init",
				Image: initContainerImage,
				VolumeMounts: []corev1.VolumeMount{
					dseConfigVolumeMount,
				},
				Env: []corev1.EnvVar{
					{
						Name:  "CONFIG_FILE_DATA",
						Value: configData,
					},
					{
						Name: "POD_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
					{
						Name: "RACK_NAME",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: fmt.Sprintf("metadata.labels['%s']", datastaxv1alpha1.RackLabel),
							},
						},
					},
					{
						Name:  "DSE_VERSION",
						Value: dseVersion,
					},
				},
			}},
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{
					Name:      "dse",
					Image:     dseImage,
					Resources: dseDatacenter.Spec.Resources,
					Env: []corev1.EnvVar{
						{
							Name:  "DS_LICENSE",
							Value: "accept",
						},
						{
							Name:  "DSE_AUTO_CONF_OFF",
							Value: "all",
						},
						{
							Name:  "USE_MGMT_API",
							Value: "true",
						},
						{
							Name:  "DSE_MGMT_EXPLICIT_START",
							Value: "true",
						},
					},
					Ports: ports,
					LivenessProbe: &corev1.Probe{
						Handler: corev1.Handler{
							HTTPGet: &corev1.HTTPGetAction{
								Port: intstr.FromInt(8080),
								Path: "/api/v0/probes/liveness",
							},
						},
						InitialDelaySeconds: 15,
						PeriodSeconds:       15,
					},
					ReadinessProbe: &corev1.Probe{
						Handler: corev1.Handler{
							HTTPGet: &corev1.HTTPGetAction{
								Port: intstr.FromInt(8080),
								Path: "/api/v0/probes/readiness",
							},
						},
						InitialDelaySeconds: 20,
						PeriodSeconds:       10,
					},
					VolumeMounts: dseVolumeMounts,
				},
				{
					Name:  "dse-system-logger",
					Image: "busybox",
					Args: []string{
						"/bin/sh", "-c", "tail -n+1 -F /var/log/cassandra/system.log",
					},
					VolumeMounts: []corev1.VolumeMount{
						corev1.VolumeMount{
							Name:      "dse-logs",
							MountPath: "/var/log/cassandra",
						},
					},
				},
			},
		},
	}

	_ = httphelper.AddManagementApiServerSecurity(dseDatacenter, &template)

	result := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nsName.Name,
			Namespace: nsName.Namespace,
			Labels:    statefulSetLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: statefulSetSelectorLabels,
			},
			Replicas:             &replicaCountInt32,
			ServiceName:          dseDatacenter.GetDseDatacenterServiceName(),
			PodManagementPolicy:  appsv1.ParallelPodManagement,
			Template:             template,
			VolumeClaimTemplates: volumeCaimTemplates,
		},
	}

	return result, nil
}

// Create a PodDisruptionBudget object for the DSE Datacenter
func newPodDisruptionBudgetForDatacenter(dseDatacenter *datastaxv1alpha1.DseDatacenter) *policyv1beta1.PodDisruptionBudget {
	minAvailable := intstr.FromInt(int(dseDatacenter.Spec.Size - 1))
	labels := dseDatacenter.GetDatacenterLabels()
	oplabels.AddManagedByLabel(labels)
	selectorLabels := dseDatacenter.GetDatacenterLabels()
	return &policyv1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dseDatacenter.Name + "-pdb",
			Namespace: dseDatacenter.Namespace,
			Labels:    labels,
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			MinAvailable: &minAvailable,
		},
	}
}

// this type exists so there's no chance of pushing random strings to our progress label
type dseOperatorStatus string

const (
	updating dseOperatorStatus = "Updating"
	ready    dseOperatorStatus = "Ready"
)

func addOperatorProgressLabel(
	rc *dsereconciliation.ReconciliationContext,
	status dseOperatorStatus) error {

	labelVal := string(status)

	dcLabels := rc.DseDatacenter.GetLabels()
	if dcLabels == nil {
		dcLabels = make(map[string]string)
	}

	if dcLabels[datastaxv1alpha1.DseOperatorProgressLabel] == labelVal {
		// early return, no need to ping k8s
		return nil
	}

	// set the label and push it to k8s
	dcLabels[datastaxv1alpha1.DseOperatorProgressLabel] = labelVal
	rc.DseDatacenter.SetLabels(dcLabels)
	if err := rc.Client.Update(rc.Ctx, rc.DseDatacenter); err != nil {
		rc.ReqLogger.Error(err, "error updating label",
			"label", datastaxv1alpha1.DseOperatorProgressLabel,
			"value", labelVal)
		return err
	}

	return nil
}

// calculateNodeAffinity provides a way to pin all pods within a statefulset to the same zone
func calculateNodeAffinity(zone string) *corev1.NodeAffinity {
	if zone == "" {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "failure-domain.beta.kubernetes.io/zone",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{zone},
						},
					},
				},
			},
		},
	}
}

// calculatePodAntiAffinity provides a way to keep the dse pods of a statefulset away from other dse pods
func calculatePodAntiAffinity(allowMultipleNodesPerWorker bool) *corev1.PodAntiAffinity {
	if allowMultipleNodesPerWorker {
		return nil
	}
	return &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      datastaxv1alpha1.ClusterLabel,
							Operator: metav1.LabelSelectorOpExists,
						},
						{
							Key:      datastaxv1alpha1.DatacenterLabel,
							Operator: metav1.LabelSelectorOpExists,
						},
						{
							Key:      datastaxv1alpha1.RackLabel,
							Operator: metav1.LabelSelectorOpExists,
						},
					},
				},
				TopologyKey: "kubernetes.io/hostname",
			},
		},
	}
}
