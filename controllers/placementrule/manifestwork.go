// Copyright (c) 2021 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package placementrule

import (
	"context"
	"errors"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workv1 "github.com/open-cluster-management/api/work/v1"
	mcoshared "github.com/open-cluster-management/multicluster-observability-operator/api/shared"
	mcov1beta1 "github.com/open-cluster-management/multicluster-observability-operator/api/v1beta1"
	mcov1beta2 "github.com/open-cluster-management/multicluster-observability-operator/api/v1beta2"
	mcoctrl "github.com/open-cluster-management/multicluster-observability-operator/controllers/multiclusterobservability"
	"github.com/open-cluster-management/multicluster-observability-operator/pkg/config"
	"github.com/open-cluster-management/multicluster-observability-operator/pkg/util"
)

const (
	operatorWorkNameSuffix = "-observability-operator"
	resWorkNameSuffix      = "-observability-operator-res"
	localClusterName       = "local-cluster"
)

type MetricsAllowlist struct {
	NameList  []string `yaml:"names"`
	MatchList []string `yaml:"matches"`
}

func deleteManifestWork(c client.Client, name string, namespace string) error {

	addon := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	err := c.Delete(context.TODO(), addon)
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Error(err, "Failed to delete manifestworks", "name", name, "namespace", namespace)
		return err
	}
	return nil
}

func deleteManifestWorks(c client.Client, namespace string) error {
	err := deleteRes(c, namespace)
	if err != nil {
		return err
	}

	err = c.DeleteAllOf(context.TODO(), &workv1.ManifestWork{},
		client.InNamespace(namespace), client.MatchingLabels{ownerLabelKey: ownerLabelValue})
	if err != nil {
		log.Error(err, "Failed to delete observability manifestworks", "namespace", namespace)
	}
	return err
}

func injectIntoWork(works []workv1.Manifest, obj runtime.Object) []workv1.Manifest {
	works = append(works,
		workv1.Manifest{
			RawExtension: runtime.RawExtension{
				Object: obj,
			},
		})
	return works
}

func newManifestwork(name string, namespace string) *workv1.ManifestWork {
	return &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ownerLabelKey: ownerLabelValue,
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Workload: workv1.ManifestsTemplate{
				Manifests: []workv1.Manifest{},
			},
		},
	}
}

func createManifestwork(c client.Client, work *workv1.ManifestWork) error {
	name := work.ObjectMeta.Name
	namespace := work.ObjectMeta.Namespace
	found := &workv1.ManifestWork{}
	err := c.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, found)
	if err != nil && k8serrors.IsNotFound(err) {
		log.Info("Creating manifestwork", "namespace", namespace, "name", name)

		err = c.Create(context.TODO(), work)
		if err != nil {
			log.Error(err, "Failed to create manifestwork", "namespace", namespace, "name", name)
			return err
		}
		return nil
	} else if err != nil {
		log.Error(err, "Failed to check manifestwork", namespace, "name", name)
		return err
	}

	if found.GetDeletionTimestamp() != nil {
		log.Info("Existing manifestwork is terminating, skip and reconcile later")
		return errors.New("Existing manifestwork is terminating, skip and reconcile later")
	}

	manifests := work.Spec.Workload.Manifests
	updated := false
	if len(found.Spec.Workload.Manifests) == len(manifests) {
		for i, m := range found.Spec.Workload.Manifests {
			if !util.CompareObject(m.RawExtension, manifests[i].RawExtension) {
				updated = true
				break
			}
		}
	} else {
		updated = true
	}

	if updated {
		log.Info("Updating manifestwork", namespace, namespace, "name", name)
		work.ObjectMeta.ResourceVersion = found.ObjectMeta.ResourceVersion
		err = c.Update(context.TODO(), work)
		if err != nil {
			log.Error(err, "Failed to update monitoring-endpoint-monitoring-work work")
			return err
		}
		return nil
	}

	log.Info("manifestwork already existed/unchanged", "namespace", namespace)
	return nil
}

func createManifestWorks(c client.Client, restMapper meta.RESTMapper,
	clusterNamespace string, clusterName string,
	mco *mcov1beta2.MultiClusterObservability,
	imagePullSecret *corev1.Secret) error {

	operatorWork := newManifestwork(clusterNamespace+operatorWorkNameSuffix, clusterNamespace)

	// inject resouces in templates
	templates, err := loadTemplates(clusterNamespace, mco)
	if err != nil {
		log.Error(err, "Failed to load templates")
		return err
	}
	for _, raw := range templates {
		if clusterName == localClusterName &&
			raw.Object == nil {
			//raw.Object.GetObjectKind().GroupVersionKind().Kind == "CustomResourceDefinition" {
			continue
		}
		operatorWork.Spec.Workload.Manifests = append(
			operatorWork.Spec.Workload.Manifests,
			workv1.Manifest{RawExtension: raw})
	}

	err = createManifestwork(c, operatorWork)
	if err != nil {
		return err
	}

	resourceWork := newManifestwork(clusterNamespace+resWorkNameSuffix, clusterNamespace)
	manifests := resourceWork.Spec.Workload.Manifests
	// inject observabilityAddon
	obaddon, err := getObservabilityAddon(c, clusterNamespace, mco)
	if err != nil {
		return err
	}
	if obaddon != nil {
		manifests = injectIntoWork(manifests, obaddon)
	}

	// inject the hub info secret
	hubInfo, err := newHubInfoSecret(c, config.GetDefaultNamespace(), spokeNameSpace, clusterName, mco)
	if err != nil {
		return err
	}
	manifests = injectIntoWork(manifests, hubInfo)

	// inject namespace
	manifests = injectIntoWork(manifests, createNameSpace())

	// inject kube secret
	secret, err := createKubeSecret(c, restMapper, clusterNamespace)
	if err != nil {
		return err
	}
	manifests = injectIntoWork(manifests, secret)

	//create image pull secret
	if imagePullSecret != nil {
		pull := getPullSecret(imagePullSecret)
		manifests = injectIntoWork(manifests, pull)
	}

	// inject the certificates
	certs, err := getCerts(c, clusterNamespace)
	if err != nil {
		return err
	}
	manifests = injectIntoWork(manifests, certs)

	// inject the metrics allowlist configmap
	mList, err := getMetricsListCM(c)
	if err != nil {
		return err
	}
	manifests = injectIntoWork(manifests, mList)

	resourceWork.Spec.Workload.Manifests = manifests

	err = createManifestwork(c, resourceWork)
	return err
}

func getPullSecret(imagePullSecret *corev1.Secret) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      imagePullSecret.Name,
			Namespace: spokeNameSpace,
		},
		Data: map[string][]byte{
			".dockerconfigjson": imagePullSecret.Data[".dockerconfigjson"],
		},
		Type: corev1.SecretTypeDockerConfigJson,
	}
}

func getCerts(client client.Client, namespace string) (*corev1.Secret, error) {

	ca := &corev1.Secret{}
	caName := mcoctrl.GetServerCerts()
	err := client.Get(context.TODO(), types.NamespacedName{Name: caName,
		Namespace: config.GetDefaultNamespace()}, ca)
	if err != nil {
		log.Error(err, "Failed to get ca cert secret", "name", caName)
		return nil, err
	}

	certs := &corev1.Secret{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: certsName, Namespace: namespace}, certs)
	if err != nil {
		log.Error(err, "Failed to get certs secret", "name", certsName, "namespace", namespace)
		return nil, err
	}

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      certsName,
			Namespace: spokeNameSpace,
		},
		Data: map[string][]byte{
			"ca.crt":  ca.Data["ca.crt"],
			"tls.crt": certs.Data["tls.crt"],
			"tls.key": certs.Data["tls.key"],
		},
	}, nil
}

func getMetricsListCM(client client.Client) (*corev1.ConfigMap, error) {
	metricsAllowlist := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.AllowlistConfigMapName,
			Namespace: spokeNameSpace,
		},
		Data: map[string]string{},
	}

	allowlist, err := getAllowList(client, config.AllowlistConfigMapName)
	if err != nil {
		log.Error(err, "Failed to get metrics allowlist configmap "+config.AllowlistConfigMapName)
		return nil, err
	}

	customAllowlist, err := getAllowList(client, config.AllowlistCustomConfigMapName)
	if err == nil {
		allowlist.NameList = append(allowlist.NameList, customAllowlist.NameList...)
		allowlist.MatchList = append(allowlist.MatchList, customAllowlist.MatchList...)
	} else {
		log.Info("There is no custom metrics allowlist configmap in the cluster")
	}

	data, err := yaml.Marshal(allowlist)
	if err != nil {
		log.Error(err, "Failed to marshal allowlist data")
		return nil, err
	}
	metricsAllowlist.Data["metrics_list.yaml"] = string(data)
	return metricsAllowlist, nil
}

func getAllowList(client client.Client, name string) (*MetricsAllowlist, error) {
	found := &corev1.ConfigMap{}
	namespacedName := types.NamespacedName{
		Name:      name,
		Namespace: config.GetDefaultNamespace(),
	}
	err := client.Get(context.TODO(), namespacedName, found)
	if err != nil {
		return nil, err
	}
	allowlist := &MetricsAllowlist{}
	err = yaml.Unmarshal([]byte(found.Data["metrics_list.yaml"]), allowlist)
	if err != nil {
		log.Error(err, "Failed to unmarshal data in configmap "+name)
		return nil, err
	}
	return allowlist, nil
}

func getObservabilityAddon(c client.Client, namespace string,
	mco *mcov1beta2.MultiClusterObservability) (*mcov1beta1.ObservabilityAddon, error) {
	found := &mcov1beta1.ObservabilityAddon{}
	namespacedName := types.NamespacedName{
		Name:      obsAddonName,
		Namespace: namespace,
	}
	err := c.Get(context.TODO(), namespacedName, found)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		log.Error(err, "Failed to check observabilityAddon")
		return nil, err
	}
	if found.ObjectMeta.DeletionTimestamp != nil {
		return nil, nil
	}
	if mco.Spec.ObservabilityAddonSpec == nil {
		mco.Spec.ObservabilityAddonSpec = &mcoshared.ObservabilityAddonSpec{
			EnableMetrics: true,
			Interval:      30,
		}
	}
	return &mcov1beta1.ObservabilityAddon{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "observability.open-cluster-management.io/v1beta1",
			Kind:       "ObservabilityAddon",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      obsAddonName,
			Namespace: spokeNameSpace,
		},
		Spec: mcoshared.ObservabilityAddonSpec{
			EnableMetrics: mco.Spec.ObservabilityAddonSpec.EnableMetrics,
			Interval:      mco.Spec.ObservabilityAddonSpec.Interval,
		},
	}, nil
}

func removeObservabilityAddon(client client.Client, namespace string) error {
	name := namespace + resWorkNameSuffix
	found := &workv1.ManifestWork{}
	err := client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, found)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "Failed to check manifestwork", "namespace", namespace, "name", name)
		return err
	}

	obj, err := util.GetObject(found.Spec.Workload.Manifests[0].RawExtension)
	if err != nil {
		return err
	}
	if obj.GetObjectKind().GroupVersionKind().Kind == "ObservabilityAddon" {
		updateManifests := found.Spec.Workload.Manifests[1:]
		found.Spec.Workload.Manifests = updateManifests

		err = client.Update(context.TODO(), found)
		if err != nil {
			log.Error(err, "Failed to update manifestwork", "namespace", namespace, "name", name)
			return err
		}
	}
	return nil
}
