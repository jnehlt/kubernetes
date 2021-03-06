/*
Copyright 2017 The Kubernetes Authors.

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

package etcd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/klog"

	"k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/images"
	kubeadmutil "k8s.io/kubernetes/cmd/kubeadm/app/util"
	etcdutil "k8s.io/kubernetes/cmd/kubeadm/app/util/etcd"
	staticpodutil "k8s.io/kubernetes/cmd/kubeadm/app/util/staticpod"
)

const (
	etcdVolumeName  = "etcd-data"
	certsVolumeName = "etcd-certs"
)

// CreateLocalEtcdStaticPodManifestFile will write local etcd static pod manifest file.
// This function is used by init - when the etcd cluster is empty - or by kubeadm
// upgrade - when the etcd cluster is already up and running (and the --initial-cluster flag have no impact)
func CreateLocalEtcdStaticPodManifestFile(manifestDir string, cfg *kubeadmapi.InitConfiguration) error {
	if cfg.ClusterConfiguration.Etcd.External != nil {
		return errors.New("etcd static pod manifest cannot be generated for cluster using external etcd")
	}
	// gets etcd StaticPodSpec
	emptyInitialCluster := []etcdutil.Member{}
	spec := GetEtcdPodSpec(cfg, emptyInitialCluster)
	// writes etcd StaticPod to disk
	if err := staticpodutil.WriteStaticPodToDisk(kubeadmconstants.Etcd, manifestDir, spec); err != nil {
		return err
	}

	klog.V(1).Infof("[etcd] wrote Static Pod manifest for a local etcd instance to %q\n", kubeadmconstants.GetStaticPodFilepath(kubeadmconstants.Etcd, manifestDir))
	return nil
}

// CheckLocalEtcdClusterStatus verifies health state of local/stacked etcd cluster before installing a new etcd member
func CheckLocalEtcdClusterStatus(client clientset.Interface, cfg *kubeadmapi.InitConfiguration) error {
	fmt.Println("[etcd] Checking Etcd cluster health")

	// creates an etcd client that connects to all the local/stacked etcd members
	klog.V(1).Info("creating etcd client that connects to etcd pods")
	etcdClient, err := etcdutil.NewFromCluster(client, cfg.CertificatesDir)
	if err != nil {
		return err
	}

	// Checking health state
	_, err = etcdClient.GetClusterStatus()
	if err != nil {
		return errors.Wrap(err, "etcd cluster is not healthy")
	}

	return nil
}

// CreateStackedEtcdStaticPodManifestFile will write local etcd static pod manifest file
// for an additional etcd member that is joining an existing local/stacked etcd cluster.
// Other members of the etcd cluster will be notified of the joining node in beforehand as well.
func CreateStackedEtcdStaticPodManifestFile(client clientset.Interface, manifestDir string, cfg *kubeadmapi.InitConfiguration) error {
	// creates an etcd client that connects to all the local/stacked etcd members
	klog.V(1).Info("creating etcd client that connects to etcd pods")
	etcdClient, err := etcdutil.NewFromCluster(client, cfg.CertificatesDir)
	if err != nil {
		return err
	}

	// notifies the other members of the etcd cluster about the joining member
	etcdPeerAddress := etcdutil.GetPeerURL(cfg)

	klog.V(1).Infof("Adding etcd member: %s", etcdPeerAddress)
	initialCluster, err := etcdClient.AddMember(cfg.NodeRegistration.Name, etcdPeerAddress)
	if err != nil {
		return err
	}
	fmt.Println("[etcd] Announced new etcd member joining to the existing etcd cluster")
	klog.V(1).Infof("Updated etcd member list: %v", initialCluster)

	klog.V(1).Info("Creating local etcd static pod manifest file")
	// gets etcd StaticPodSpec, actualized for the current InitConfiguration and the new list of etcd members
	spec := GetEtcdPodSpec(cfg, initialCluster)
	// writes etcd StaticPod to disk
	if err := staticpodutil.WriteStaticPodToDisk(kubeadmconstants.Etcd, manifestDir, spec); err != nil {
		return err
	}

	fmt.Printf("[etcd] Wrote Static Pod manifest for a local etcd instance to %q\n", kubeadmconstants.GetStaticPodFilepath(kubeadmconstants.Etcd, manifestDir))
	return nil
}

// GetEtcdPodSpec returns the etcd static Pod actualized to the context of the current InitConfiguration
// NB. GetEtcdPodSpec methods holds the information about how kubeadm creates etcd static pod manifests.
func GetEtcdPodSpec(cfg *kubeadmapi.InitConfiguration, initialCluster []etcdutil.Member) v1.Pod {
	pathType := v1.HostPathDirectoryOrCreate
	etcdMounts := map[string]v1.Volume{
		etcdVolumeName:  staticpodutil.NewVolume(etcdVolumeName, cfg.Etcd.Local.DataDir, &pathType),
		certsVolumeName: staticpodutil.NewVolume(certsVolumeName, cfg.CertificatesDir+"/etcd", &pathType),
	}
	return staticpodutil.ComponentPod(v1.Container{
		Name:            kubeadmconstants.Etcd,
		Command:         getEtcdCommand(cfg, initialCluster),
		Image:           images.GetEtcdImage(&cfg.ClusterConfiguration),
		ImagePullPolicy: v1.PullIfNotPresent,
		// Mount the etcd datadir path read-write so etcd can store data in a more persistent manner
		VolumeMounts: []v1.VolumeMount{
			staticpodutil.NewVolumeMount(etcdVolumeName, cfg.Etcd.Local.DataDir, false),
			staticpodutil.NewVolumeMount(certsVolumeName, cfg.CertificatesDir+"/etcd", false),
		},
		LivenessProbe: staticpodutil.EtcdProbe(
			cfg, kubeadmconstants.Etcd, kubeadmconstants.EtcdListenClientPort, cfg.CertificatesDir,
			kubeadmconstants.EtcdCACertName, kubeadmconstants.EtcdHealthcheckClientCertName, kubeadmconstants.EtcdHealthcheckClientKeyName,
		),
	}, etcdMounts)
}

// getEtcdCommand builds the right etcd command from the given config object
func getEtcdCommand(cfg *kubeadmapi.InitConfiguration, initialCluster []etcdutil.Member) []string {
	defaultArguments := map[string]string{
		"name":                        cfg.GetNodeName(),
		"listen-client-urls":          fmt.Sprintf("%s,%s", etcdutil.GetClientURLByIP("127.0.0.1"), etcdutil.GetClientURL(cfg)),
		"advertise-client-urls":       etcdutil.GetClientURL(cfg),
		"listen-peer-urls":            etcdutil.GetPeerURL(cfg),
		"initial-advertise-peer-urls": etcdutil.GetPeerURL(cfg),
		"data-dir":                    cfg.Etcd.Local.DataDir,
		"cert-file":                   filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdServerCertName),
		"key-file":                    filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdServerKeyName),
		"trusted-ca-file":             filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdCACertName),
		"client-cert-auth":            "true",
		"peer-cert-file":              filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdPeerCertName),
		"peer-key-file":               filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdPeerKeyName),
		"peer-trusted-ca-file":        filepath.Join(cfg.CertificatesDir, kubeadmconstants.EtcdCACertName),
		"peer-client-cert-auth":       "true",
		"snapshot-count":              "10000",
	}

	if len(initialCluster) == 0 {
		defaultArguments["initial-cluster"] = fmt.Sprintf("%s=%s", cfg.GetNodeName(), etcdutil.GetPeerURL(cfg))
	} else {
		// NB. the joining etcd instance should be part of the initialCluster list
		endpoints := []string{}
		for _, member := range initialCluster {
			endpoints = append(endpoints, fmt.Sprintf("%s=%s", member.Name, member.PeerURL))
		}

		defaultArguments["initial-cluster"] = strings.Join(endpoints, ",")
		defaultArguments["initial-cluster-state"] = "existing"
	}

	command := []string{"etcd"}
	command = append(command, kubeadmutil.BuildArgumentListFromMap(defaultArguments, cfg.Etcd.Local.ExtraArgs)...)
	return command
}
