// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap.com/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/label"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

func NewOperatorActions(cli versioned.Interface, kubeCli kubernetes.Interface) OperatorActions {
	return &operatorActions{
		cli:       cli,
		kubeCli:   kubeCli,
		pdControl: controller.NewDefaultPDControl(),
	}
}

type OperatorActions interface {
	DeployOperator(info *OperatorInfo) error
	CleanOperator(info *OperatorInfo) error
	UpgradeOperator(info *OperatorInfo) error
	DumpAllLogs(info *OperatorInfo, clusterInfo *TidbClusterInfo) error
	DeployTidbCluster(info *TidbClusterInfo) error
	CleanTidbCluster(info *TidbClusterInfo) error
	CheckTidbClusterStatus(info *TidbClusterInfo) error
	BeginInsertDataTo(info *TidbClusterInfo) error
	StopInsertDataTo(info *TidbClusterInfo) error
	ScaleTidbCluster(info *TidbClusterInfo) error
	UpgradeTidbCluster(info *TidbClusterInfo) error
	DeployAdHocBackup(info *TidbClusterInfo) error
	CleanAdHocBackup(info *TidbClusterInfo) error
	DeployScheduledBackup(info *TidbClusterInfo) error
	CleanScheduledBackup(info *TidbClusterInfo) error
	DeployIncrementalBackup(info *TidbClusterInfo) error
	CleanIncrementalBackup(info *TidbClusterInfo) error
	Restore(from *TidbClusterInfo, jobName string, to *TidbClusterInfo) error
	DeployMonitor(info *TidbClusterInfo) error
	CleanMonitor(info *TidbClusterInfo) error
}

type FaultTriggerActions interface {
	StopNode(nodeName string) error
	StartNode(nodeName string) error
	StopEtcd() error
	StartEtcd() error
	StopKubeAPIServer() error
	StartKubeAPIServer() error
	StopKubeControllerManager() error
	StartKubeControllerManager() error
	StopKubeScheduler() error
	StartKubeScheduler() error
	StopKubelet(nodeName string) error
	StartKubelet(nodeName string) error
	StopKubeProxy(nodeName string) error
	StartKubeProxy(nodeName string) error
	DiskCorruption(nodeName string) error
	NetworkPartition(fromNode, toNode string) error
	NetworkDelay(fromNode, toNode string) error
	DockerCrash(nodeName string) error
}

type operatorActions struct {
	cli       versioned.Interface
	kubeCli   kubernetes.Interface
	pdControl controller.PDControlInterface
}

type OperatorInfo struct {
	Namespace      string
	ReleaseName    string
	Image          string
	Tag            string
	SchedulerImage string
	LogLevel       string
}

type TidbClusterInfo struct {
	Namespace        string
	ClusterName      string
	OperatorTag      string
	PDImage          string
	TiKVImage        string
	TiDBImage        string
	StorageClassName string
	Password         string
	RecordCount      string
	InsertBetchSize  string
	Resources        map[string]string
	Args             map[string]string
}

func (tc *TidbClusterInfo) HelmSetString() string {
	set := map[string]string{
		"clusterName":           tc.ClusterName,
		"pd.storageClassName":   tc.StorageClassName,
		"tikv.storageClassName": tc.StorageClassName,
		"tidb.storageClassName": tc.StorageClassName,
		"tidb.password":         tc.Password,
		"pd.maxStoreDownTime":   "5m",
		"pd.image":              tc.PDImage,
		"tikv.image":            tc.TiKVImage,
		"tidb.image":            tc.TiDBImage,
	}

	for k, v := range tc.Resources {
		set[k] = v
	}
	for k, v := range tc.Args {
		set[k] = v
	}

	arr := make([]string, 0, len(set))
	for k, v := range set {
		arr = append(arr, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(arr, ",")
}

func (oa *operatorActions) DeployOperator(info *OperatorInfo) error {
	if err := cloneOperatorRepo(); err != nil {
		return err
	}
	if err := checkoutTag(info.Tag); err != nil {
		return err
	}

	deployCmd := fmt.Sprintf(`helm install /charts/%s/tidb-operator
		--name %s
		--namespace %s
		--set operatorImage=%s
		--set controllerManager.autoFailover=true
		--set scheduler.kubeSchedulerImage=%s
		--set controllerManager.logLevel=%d
		--set scheduler.logLevel=4`,
		info.Tag,
		info.ReleaseName,
		info.Namespace,
		info.Image,
		info.SchedulerImage,
		info.LogLevel)
	res, err := exec.Command("/bin/sh", "-c", deployCmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to deploy operator: %v, %s", err, string(res))
	}

	return nil
}

func (oa *operatorActions) CleanOperator(info *OperatorInfo) error {
	res, err := exec.Command("helm", "del", "--purge", info.ReleaseName).CombinedOutput()
	if err == nil || releaseIsNotFound(err) {
		return nil
	}
	return fmt.Errorf("failed to clear operator: %v, %s", err, string(res))
}

func (oa *operatorActions) UpgradeOperator(info *OperatorInfo) error {
	if err := checkoutTag(info.Tag); err != nil {
		return err
	}

	cmd := fmt.Sprintf(`helm upgrade %s /charts/%s/tidb-operator
		--set operatorImage=%s`,
		info.ReleaseName, info.Tag,
		info.Image)
	res, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to upgrade operator to: %s, %v, %s", info.Image, err, string(res))
	}
	return nil
}

func (oa *operatorActions) DumpAllLogs(info *OperatorInfo, clusterInfo *TidbClusterInfo) error {
	return nil
}

func (oa *operatorActions) DeployTidbCluster(info *TidbClusterInfo) error {
	cmd := fmt.Sprintf("helm install /charts/%s/tidb-cluster  --name %s --namespace %s --set-string %s",
		info.OperatorTag, info.ClusterName, info.Namespace, info.HelmSetString())
	if res, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to deploy tidbcluster: %s/%s, %v, %s",
			info.Namespace, info.ClusterName, err, string(res))
	}

	return nil
}

func (oa *operatorActions) CleanTidbCluster(info *TidbClusterInfo) error {
	charts := []string{
		info.ClusterName,
		fmt.Sprintf("%s-backup", info.ClusterName),
		fmt.Sprintf("%s-restore", info.ClusterName),
	}
	for _, chartName := range charts {
		res, err := exec.Command("helm", "del", "--purge", chartName).CombinedOutput()
		if err != nil && !releaseIsNotFound(err) {
			return fmt.Errorf("failed to delete chart: %s/%s, %v, %s",
				info.Namespace, chartName, err, string(res))
		}
	}

	resources := []string{"cronjobs", "jobs", "pods", "pvcs"}
	for _, resource := range resources {
		if res, err := exec.Command("kubectl", "delete", resource, "-n", info.Namespace,
			"--all").CombinedOutput(); err != nil {
			return fmt.Errorf("failed to delete %s: %v, %s", resource, err, string(res))
		}
	}

	patchPVCmd := fmt.Sprintf(`kubectl get pv -l %s=%s,%s=%s --output=name | xargs -I {}
		kubectl patch {} -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'`,
		label.NamespaceLabelKey, info.Namespace, label.InstanceLabelKey, info.ClusterName)
	if res, err := exec.Command("/bin/sh", "-c", patchPVCmd).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to patch pv: %v, %s", err, string(res))
	}

	pollFn := func() (bool, error) {
		if res, err := exec.Command("kubectl", "get", "po", "--output=name", "-n", info.Namespace).
			CombinedOutput(); err != nil || len(res) == 0 {
			glog.Infof("waiting for tidbcluster: %s/%s pods deleting\n",
				info.Namespace, info.ClusterName, string(res))
			return false, nil
		}

		pvCmd := fmt.Sprintf("kubectl get pv -l %s=%s,%s=%s 2>/dev/null|grep Released",
			label.NamespaceLabelKey, info.Namespace, label.InstanceLabelKey, info.ClusterName)
		if res, err := exec.Command("/bin/sh", "-c", pvCmd).
			CombinedOutput(); err != nil || len(res) != 0 {
			glog.Infof("waiting for tidbcluster: %s/%s pvs deleting\n%s",
				info.Namespace, info.ClusterName, string(res))
			return false, nil
		}

		return true, nil
	}
	return wait.PollImmediate(1*time.Minute, 5*time.Minute, pollFn)
}

func (oa *operatorActions) CheckTidbClusterStatus(info *TidbClusterInfo) error {
	ns := info.Namespace
	tcName := info.ClusterName
	var tc *v1alpha1.TidbCluster
	var err error
	if tc, err = oa.cli.PingcapV1alpha1().TidbClusters(ns).Get(tcName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("failed to get tidbcluster: %s/%s, %v", ns, tcName, err)
	}

	if err := wait.PollImmediate(1*time.Minute, 5*time.Minute, func() (bool, error) {
		if b, err := oa.pdMembersReadyFn(tc); !b && err == nil {
			return false, nil
		}
		if b, err := oa.tikvMembersReadyFn(tc); !b && err == nil {
			return false, nil
		}
		if b, err := oa.tidbMembersReadyFn(tc); !b && err == nil {
			return false, nil
		}
		if b, err := oa.reclaimPolicySyncFn(tc); !b && err == nil {
			return false, nil
		}
		if b, err := oa.metaSyncFn(tc); err != nil {
			return false, err
		} else if !b && err == nil {
			return false, nil
		}
		if b, err := oa.schedulerHAFn(tc); !b && err == nil {
			return false, nil
		}
		if b, err := oa.passwordIsSet(info); !b && err == nil {
			return false, nil
		}

		return true, nil
	}); err != nil {
		return fmt.Errorf("failed to waiting for tidbcluster %s/%s ready in 5 minutes", ns, tcName)
	}

	return nil
}

func (oa *operatorActions) BeginInsertDataTo(info *TidbClusterInfo) error {
	return nil
}

func (oa *operatorActions) StopInsertDataTo(info *TidbClusterInfo) error {
	return nil
}

func (oa *operatorActions) ScaleTidbCluster(info *TidbClusterInfo) error        { return nil }
func (oa *operatorActions) UpgradeTidbCluster(info *TidbClusterInfo) error      { return nil }
func (oa *operatorActions) DeployAdHocBackup(info *TidbClusterInfo) error       { return nil }
func (oa *operatorActions) CleanAdHocBackup(info *TidbClusterInfo) error        { return nil }
func (oa *operatorActions) DeployScheduledBackup(info *TidbClusterInfo) error   { return nil }
func (oa *operatorActions) CleanScheduledBackup(info *TidbClusterInfo) error    { return nil }
func (oa *operatorActions) DeployIncrementalBackup(info *TidbClusterInfo) error { return nil }
func (oa *operatorActions) CleanIncrementalBackup(info *TidbClusterInfo) error  { return nil }
func (oa *operatorActions) Restore(from *TidbClusterInfo, jobName string, to *TidbClusterInfo) error {
	return nil
}
func (oa *operatorActions) DeployMonitor(info *TidbClusterInfo) error { return nil }
func (oa *operatorActions) CleanMonitor(info *TidbClusterInfo) error  { return nil }

func (oa *operatorActions) pdMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	pdSetName := controller.PDMemberName(tcName)

	pdSet, err := oa.kubeCli.AppsV1beta1().StatefulSets(ns).Get(pdSetName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get statefulset: %s/%s, %v", ns, pdSetName, err)
		return false, nil
	}

	if tc.Status.PD.StatefulSet == nil {
		glog.Infof("tidbcluster: %s/%s .status.PD.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.PD.FailureMembers)
	replicas := tc.Spec.PD.Replicas + int32(failureCount)
	if *pdSet.Spec.Replicas != replicas {
		glog.Infof("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, pdSetName, *pdSet.Spec.Replicas, ns, tcName, replicas)
		return false, nil
	}
	if pdSet.Status.ReadyReplicas != tc.Spec.PD.Replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, pdSetName, pdSet.Status.ReadyReplicas, tc.Spec.PD.Replicas)
		return false, nil
	}
	if len(tc.Status.PD.Members) != int(tc.Spec.PD.Replicas) {
		glog.Infof("tidbcluster: %s/%s .status.PD.Members count(%d) != %d",
			ns, tcName, len(tc.Status.PD.Members), tc.Spec.PD.Replicas)
		return false, nil
	}
	if pdSet.Status.ReadyReplicas != pdSet.Status.Replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, pdSetName, pdSet.Status.ReadyReplicas, pdSet.Status.Replicas)
		return false, nil
	}

	for _, member := range tc.Status.PD.Members {
		if !member.Health {
			glog.Infof("tidbcluster: %s/%s pd member(%s/%s) is not health",
				ns, tcName, member.ID, member.Name)
			return false, nil
		}
	}

	pdServiceName := controller.PDMemberName(tcName)
	pdPeerServiceName := controller.PDPeerMemberName(tcName)
	if _, err := oa.kubeCli.CoreV1().Services(ns).Get(pdServiceName, metav1.GetOptions{}); err != nil {
		glog.Errorf("failed to get service: %s/%s", ns, pdServiceName)
		return false, nil
	}
	if _, err := oa.kubeCli.CoreV1().Services(ns).Get(pdPeerServiceName, metav1.GetOptions{}); err != nil {
		glog.Errorf("failed to get peer service: %s/%s", ns, pdPeerServiceName)
		return false, nil
	}

	return true, nil
}

func (oa *operatorActions) tikvMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	tikvSetName := controller.TiKVMemberName(tcName)

	tikvSet, err := oa.kubeCli.AppsV1beta1().StatefulSets(ns).Get(tikvSetName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get statefulset: %s/%s, %v", ns, tikvSetName, err)
		return false, nil
	}

	if tc.Status.TiKV.StatefulSet == nil {
		glog.Infof("tidbcluster: %s/%s .status.TiKV.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.TiKV.FailureStores)
	replicas := tc.Spec.TiKV.Replicas + int32(failureCount)
	if *tikvSet.Spec.Replicas != replicas {
		glog.Infof("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, tikvSetName, *tikvSet.Spec.Replicas, replicas)
		return false, nil
	}
	if tikvSet.Status.ReadyReplicas != replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, tikvSetName, tikvSet.Status.ReadyReplicas, replicas)
		return false, nil
	}
	if len(tc.Status.TiKV.Stores) != int(replicas) {
		glog.Infof("tidbcluster: %s/%s .status.TiKV.Stores.count(%d) != %d",
			ns, tcName, len(tc.Status.TiKV.Stores), tc.Spec.TiKV.Replicas)
		return false, nil
	}
	if tikvSet.Status.ReadyReplicas != tikvSet.Status.Replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, tikvSetName, tikvSet.Status.ReadyReplicas, tikvSet.Status.Replicas)
		return false, nil
	}

	for _, store := range tc.Status.TiKV.Stores {
		if store.State != v1alpha1.TiKVStateUp {
			glog.Infof("tidbcluster: %s/%s's store(%s) state != %s", ns, tcName, store.ID, v1alpha1.TiKVStateUp)
			return false, nil
		}
	}

	tikvPeerServiceName := controller.TiKVPeerMemberName(tcName)
	if _, err := oa.kubeCli.CoreV1().Services(ns).Get(tikvPeerServiceName, metav1.GetOptions{}); err != nil {
		glog.Errorf("failed to get peer service: %s/%s", ns, tikvPeerServiceName)
		return false, nil
	}

	return true, nil
}

func (oa *operatorActions) tidbMembersReadyFn(tc *v1alpha1.TidbCluster) (bool, error) {
	tcName := tc.GetName()
	ns := tc.GetNamespace()
	tidbSetName := controller.TiDBMemberName(tcName)

	tidbSet, err := oa.kubeCli.AppsV1beta1().StatefulSets(ns).Get(tidbSetName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get statefulset: %s/%s, %v", ns, tidbSetName, err)
		return false, nil
	}

	if tc.Status.TiDB.StatefulSet == nil {
		glog.Infof("tidbcluster: %s/%s .status.TiDB.StatefulSet is nil", ns, tcName)
		return false, nil
	}
	failureCount := len(tc.Status.TiDB.FailureMembers)
	replicas := tc.Spec.TiDB.Replicas + int32(failureCount)
	if *tidbSet.Spec.Replicas != replicas {
		glog.Infof("statefulset: %s/%s .spec.Replicas(%d) != %d",
			ns, tidbSetName, *tidbSet.Spec.Replicas, replicas)
		return false, nil
	}
	if tidbSet.Status.ReadyReplicas != tc.Spec.TiDB.Replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != %d",
			ns, tidbSetName, tidbSet.Status.ReadyReplicas, replicas)
		return false, nil
	}
	if tidbSet.Status.ReadyReplicas != tidbSet.Status.Replicas {
		glog.Infof("statefulset: %s/%s .status.ReadyReplicas(%d) != .status.Replicas(%d)",
			ns, tidbSetName, tidbSet.Status.ReadyReplicas, tidbSet.Status.Replicas)
		return false, nil
	}

	_, err = oa.kubeCli.CoreV1().Services(ns).Get(tidbSetName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("failed to get service: %s/%s", ns, tidbSetName)
		return false, nil
	}

	return true, nil
}

func (oa *operatorActions) reclaimPolicySyncFn(tc *v1alpha1.TidbCluster) (bool, error) {
	ns := tc.GetNamespace()
	tcName := tc.GetName()
	listOptions := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(
			label.New().Instance(tcName).Labels(),
		).String(),
	}
	var pvcList *corev1.PersistentVolumeClaimList
	var err error
	if pvcList, err = oa.kubeCli.CoreV1().PersistentVolumeClaims(ns).List(listOptions); err != nil {
		glog.Errorf("failed to list pvs for tidbcluster %s/%s, %v", ns, tcName, err)
		return false, nil
	}

	for _, pvc := range pvcList.Items {
		pvName := pvc.Spec.VolumeName
		if pv, err := oa.kubeCli.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{}); err != nil {
			glog.Errorf("failed to get pv: %s", pvName, err)
			return false, nil
		} else if pv.Spec.PersistentVolumeReclaimPolicy != tc.Spec.PVReclaimPolicy {
			glog.Errorf("pv: %s's reclaimPolicy is not Retain", pvName)
			return false, nil
		}
	}

	return true, nil
}

func (oa *operatorActions) metaSyncFn(tc *v1alpha1.TidbCluster) (bool, error) {
	ns := tc.GetNamespace()
	tcName := tc.GetName()

	pdCli := oa.pdControl.GetPDClient(tc)
	var cluster *metapb.Cluster
	var err error
	if cluster, err = pdCli.GetCluster(); err != nil {
		glog.Errorf("failed to get cluster from pdControl: %s/%s", ns, tcName, err)
		return false, nil
	}

	clusterID := strconv.FormatUint(cluster.Id, 10)
	listOptions := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(
			label.New().Instance(tcName).Labels(),
		).String(),
	}

	var podList *corev1.PodList
	if podList, err = oa.kubeCli.CoreV1().Pods(ns).List(listOptions); err != nil {
		glog.Errorf("failed to list pods for tidbcluster %s/%s, %v", ns, tcName, err)
		return false, nil
	}

outerLoop:
	for _, pod := range podList.Items {
		podName := pod.GetName()
		if pod.Labels[label.ClusterIDLabelKey] != clusterID {
			return false, fmt.Errorf("tidbcluster %s/%s's pod %s's label %s not equals %s ",
				ns, tcName, podName, label.ClusterIDLabelKey, clusterID)
		}

		component := pod.Labels[label.ComponentLabelKey]
		switch component {
		case label.PDLabelVal:
			var memberID string
			members, err := pdCli.GetMembers()
			if err != nil {
				glog.Errorf("failed to get members for tidbcluster %s/%s, %v", ns, tcName, err)
				return false, nil
			}
			for _, member := range members.Members {
				if member.Name == podName {
					memberID = strconv.FormatUint(member.GetMemberId(), 10)
					break
				}
			}
			if memberID == "" {
				glog.Errorf("tidbcluster: %s/%s's pod %s label [%s] is empty",
					ns, tcName, podName, label.MemberIDLabelKey)
				return false, nil
			}
			if pod.Labels[label.MemberIDLabelKey] != memberID {
				return false, fmt.Errorf("tidbcluster: %s/%s's pod %s label [%s] not equals %s",
					ns, tcName, podName, label.MemberIDLabelKey, memberID)
			}
		case label.TiKVLabelVal:
			var storeID string
			stores, err := pdCli.GetStores()
			if err != nil {
				glog.Errorf("failed to get stores for tidbcluster %s/%s, %v", ns, tcName, err)
				return false, nil
			}
			for _, store := range stores.Stores {
				addr := store.Store.GetAddress()
				if strings.Split(addr, ".")[0] == podName {
					storeID = strconv.FormatUint(store.Store.GetId(), 10)
					break
				}
			}
			if storeID == "" {
				glog.Errorf("tidbcluster: %s/%s's pod %s label [%s] is empty",
					tc.GetNamespace(), tc.GetName(), podName, label.StoreIDLabelKey)
				return false, nil
			}
			if pod.Labels[label.StoreIDLabelKey] != storeID {
				return false, fmt.Errorf("tidbcluster: %s/%s's pod %s label [%s] not equals %s",
					ns, tcName, podName, label.StoreIDLabelKey, storeID)
			}
		case label.TiDBLabelVal:
			continue outerLoop
		default:
			continue outerLoop
		}

		var pvcName string
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				pvcName = vol.PersistentVolumeClaim.ClaimName
				break
			}
		}
		if pvcName == "" {
			return false, fmt.Errorf("pod: %s/%s's pvcName is empty", ns, podName)
		}

		var pvc *corev1.PersistentVolumeClaim
		if pvc, err = oa.kubeCli.CoreV1().PersistentVolumeClaims(ns).Get(pvcName, metav1.GetOptions{}); err != nil {
			glog.Errorf("failed to get pvc %s/%s for pod %s/%s", ns, pvcName, ns, podName)
			return false, nil
		}
		if pvc.Labels[label.ClusterIDLabelKey] != clusterID {
			return false, fmt.Errorf("tidbcluster: %s/%s's pvc %s label [%s] not equals %s ",
				ns, tcName, pvcName, label.ClusterIDLabelKey, clusterID)
		}
		if pvc.Labels[label.MemberIDLabelKey] != pod.Labels[label.MemberIDLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pvc %s label [%s=%s] not equals pod lablel [%s=%s]",
				ns, tcName, pvcName,
				label.MemberIDLabelKey, pvc.Labels[label.MemberIDLabelKey],
				label.MemberIDLabelKey, pod.Labels[label.MemberIDLabelKey])
		}
		if pvc.Labels[label.StoreIDLabelKey] != pod.Labels[label.StoreIDLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pvc %s label[%s=%s] not equals pod lable[%s=%s]",
				ns, tcName, pvcName,
				label.StoreIDLabelKey, pvc.Labels[label.StoreIDLabelKey],
				label.StoreIDLabelKey, pod.Labels[label.StoreIDLabelKey])
		}
		if pvc.Annotations[label.AnnPodNameKey] != podName {
			return false, fmt.Errorf("tidbcluster: %s/%s's pvc %s annotations [%s] not equals podName: %s",
				ns, tcName, pvcName, label.AnnPodNameKey, podName)
		}

		pvName := pvc.Spec.VolumeName
		var pv *corev1.PersistentVolume
		if pv, err = oa.kubeCli.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{}); err != nil {
			glog.Errorf("failed to get pv for pvc %s/%s, %v", ns, pvcName, err)
			return false, nil
		}
		if pv.Labels[label.NamespaceLabelKey] != ns {
			return false, fmt.Errorf("tidbcluster: %s/%s 's pv %s label [%s] not equals %s",
				ns, tcName, pvName, label.NamespaceLabelKey, ns)
		}
		if pv.Labels[label.ComponentLabelKey] != pod.Labels[label.ComponentLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label[%s=%s]",
				ns, tcName, pvName,
				label.ComponentLabelKey, pv.Labels[label.ComponentLabelKey],
				label.ComponentLabelKey, pod.Labels[label.ComponentLabelKey])
		}
		if pv.Labels[label.NameLabelKey] != pod.Labels[label.NameLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label [%s=%s]",
				ns, tcName, pvName,
				label.NameLabelKey, pv.Labels[label.NameLabelKey],
				label.NameLabelKey, pod.Labels[label.NameLabelKey])
		}
		if pv.Labels[label.ManagedByLabelKey] != pod.Labels[label.ManagedByLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label [%s=%s]",
				ns, tcName, pvName,
				label.ManagedByLabelKey, pv.Labels[label.ManagedByLabelKey],
				label.ManagedByLabelKey, pod.Labels[label.ManagedByLabelKey])
		}
		if pv.Labels[label.InstanceLabelKey] != pod.Labels[label.InstanceLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label [%s=%s]",
				ns, tcName, pvName,
				label.InstanceLabelKey, pv.Labels[label.InstanceLabelKey],
				label.InstanceLabelKey, pod.Labels[label.InstanceLabelKey])
		}
		if pv.Labels[label.ClusterIDLabelKey] != clusterID {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s] not equals %s",
				ns, tcName, pvName, label.ClusterIDLabelKey, clusterID)
		}
		if pv.Labels[label.MemberIDLabelKey] != pod.Labels[label.MemberIDLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label [%s=%s]",
				ns, tcName, pvName,
				label.MemberIDLabelKey, pv.Labels[label.MemberIDLabelKey],
				label.MemberIDLabelKey, pod.Labels[label.MemberIDLabelKey])
		}
		if pv.Labels[label.StoreIDLabelKey] != pod.Labels[label.StoreIDLabelKey] {
			return false, fmt.Errorf("tidbcluster: %s/%s's pv %s label [%s=%s] not equals pod label [%s=%s]",
				ns, tcName, pvName,
				label.StoreIDLabelKey, pv.Labels[label.StoreIDLabelKey],
				label.StoreIDLabelKey, pod.Labels[label.StoreIDLabelKey])
		}
		if pv.Annotations[label.AnnPodNameKey] != podName {
			return false, fmt.Errorf("tidbcluster:[%s/%s's pv %s annotations [%s] not equals %s",
				ns, tcName, pvName, label.AnnPodNameKey, podName)
		}
	}

	return true, nil
}

func (oa *operatorActions) schedulerHAFn(tc *v1alpha1.TidbCluster) (bool, error) {
	ns := tc.GetNamespace()
	tcName := tc.GetName()

	fn := func(component string) (bool, error) {
		nodeMap := make(map[string][]string)
		listOptions := metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(
				label.New().Instance(tcName).Component(component).Labels()).String(),
		}
		var podList *corev1.PodList
		var err error
		if podList, err = oa.kubeCli.CoreV1().Pods(ns).List(listOptions); err != nil {
			glog.Errorf("failed to list pods for tidbcluster %s/%s, %v", ns, tcName, err)
			return false, nil
		}

		totalCount := len(podList.Items)
		for _, pod := range podList.Items {
			nodeName := pod.Spec.NodeName
			if len(nodeMap[nodeName]) == 0 {
				nodeMap[nodeName] = make([]string, 0)
			}
			nodeMap[nodeName] = append(nodeMap[nodeName], pod.GetName())
			if len(nodeMap[nodeName]) > totalCount/2 {
				return false, fmt.Errorf("node % have %d pods, greater than %d/2",
					nodeName, len(nodeMap[nodeName]), totalCount)
			}
		}
		return false, nil
	}

	components := []string{label.PDLabelVal, label.TiKVLabelVal}
	for _, com := range components {
		if b, err := fn(com); err != nil {
			return false, err
		} else if !b && err == nil {
			return false, nil
		}
	}

	return true, nil
}

func (oa *operatorActions) passwordIsSet(clusterInfo *TidbClusterInfo) (bool, error) {
	ns := clusterInfo.Namespace
	tcName := clusterInfo.ClusterName
	jobName := tcName + "-tidb-initializer"

	var job *batchv1.Job
	var err error
	if job, err = oa.kubeCli.BatchV1().Jobs(ns).Get(jobName, metav1.GetOptions{}); err != nil {
		glog.Errorf("failed to get job %s/%s, %v", ns, jobName, err)
		return false, nil
	}
	if job.Status.Succeeded < 1 {
		glog.Errorf("tidbcluster: %s/%s password setter job not finished", ns, tcName)
		return false, nil
	}

	var db *sql.DB
	dsn := getDSN(ns, tcName, "test", clusterInfo.Password)
	if db, err = sql.Open("mysql", dsn); err != nil {
		glog.Errorf("can't open connection to mysql: %s, %v", dsn, err)
		return false, nil
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		glog.Errorf("can't connect to mysql: %s with password %s, %v", dsn, clusterInfo.Password, err)
		return false, nil
	}

	return true, nil
}

func getDSN(ns, tcName, databaseName, password string) string {
	return fmt.Sprintf("root:%s@(%s-tidb.%s:4000)/%s?charset=utf8", password, tcName, ns, databaseName)
}

func releaseIsNotFound(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

func cloneOperatorRepo() error {
	cloneCmd := fmt.Sprintf("git clone https://github.com/pingcap/tidb-operator.git /tidb-operator")
	res, err := exec.Command("/bin/sh", "-c", cloneCmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to clone tidb-operator repository: %v, %s", err, string(res))
	}

	return nil
}

func checkoutTag(tagName string) error {
	cmd := fmt.Sprintf(`cd /tidb-operator;
		git stash -u;
		git checkout %s;
		mkdir -p /charts/%s;
		cp -rf charts/tidb-operator /charts/%s/tidb-operator;
		cp -rf charts/tidb-cluster /charts/%s/tidb-cluster;
		cp -rf charts/tidb-backup /charts/%s/tidb-backup;`,
		tagName, tagName, tagName, tagName, tagName)
	res, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to check tag: %s, %v, %s", tagName, err, string(res))
	}

	return nil
}