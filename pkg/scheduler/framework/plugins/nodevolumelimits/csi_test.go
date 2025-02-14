/*
Copyright 2019 The Kubernetes Authors.

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

package nodevolumelimits

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	csitrans "k8s.io/csi-translation-lib"
	csilibplugins "k8s.io/csi-translation-lib/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/pointer"
)

const (
	ebsCSIDriverName = csilibplugins.AWSEBSDriverName
	gceCSIDriverName = csilibplugins.GCEPDDriverName

	hostpathInTreePluginName = "kubernetes.io/hostpath"
)

var (
	scName = "csi-sc"
)

// getVolumeLimitKey returns a ResourceName by filter type
func getVolumeLimitKey(filterType string) v1.ResourceName {
	switch filterType {
	case ebsVolumeFilterType:
		return v1.ResourceName(volumeutil.EBSVolumeLimitKey)
	case gcePDVolumeFilterType:
		return v1.ResourceName(volumeutil.GCEVolumeLimitKey)
	case azureDiskVolumeFilterType:
		return v1.ResourceName(volumeutil.AzureVolumeLimitKey)
	case cinderVolumeFilterType:
		return v1.ResourceName(volumeutil.CinderVolumeLimitKey)
	default:
		return v1.ResourceName(volumeutil.GetCSIAttachLimitKey(filterType))
	}
}

func TestCSILimits(t *testing.T) {
	runningPod := st.MakePod().PVC("csi-ebs.csi.aws.com-3").Obj()
	pendingVolumePod := st.MakePod().PVC("csi-4").Obj()

	// Different pod than pendingVolumePod, but using the same unbound PVC
	unboundPVCPod2 := st.MakePod().PVC("csi-4").Obj()

	missingPVPod := st.MakePod().PVC("csi-6").Obj()
	noSCPVCPod := st.MakePod().PVC("csi-5").Obj()

	gceTwoVolPod := st.MakePod().PVC("csi-pd.csi.storage.gke.io-1").PVC("csi-pd.csi.storage.gke.io-2").Obj()

	// In-tree volumes
	inTreeOneVolPod := st.MakePod().PVC("csi-kubernetes.io/aws-ebs-0").Obj()
	inTreeTwoVolPod := st.MakePod().PVC("csi-kubernetes.io/aws-ebs-1").PVC("csi-kubernetes.io/aws-ebs-2").Obj()

	// pods with matching csi driver names
	csiEBSOneVolPod := st.MakePod().PVC("csi-ebs.csi.aws.com-0").Obj()
	csiEBSTwoVolPod := st.MakePod().PVC("csi-ebs.csi.aws.com-1").PVC("csi-ebs.csi.aws.com-2").Obj()

	inTreeNonMigratableOneVolPod := st.MakePod().PVC("csi-kubernetes.io/hostpath-0").Obj()

	ephemeralVolumePod := st.MakePod().Name("abc").Namespace("test").UID("12345").Volume(
		v1.Volume{
			Name: "xyz",
			VolumeSource: v1.VolumeSource{
				Ephemeral: &v1.EphemeralVolumeSource{},
			},
		}).Obj()

	controller := true
	ephemeralClaim := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralVolumePod.Namespace,
			Name:      ephemeralVolumePod.Name + "-" + ephemeralVolumePod.Spec.Volumes[0].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralVolumePod.Name,
					UID:        ephemeralVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	conflictingClaim := ephemeralClaim.DeepCopy()
	conflictingClaim.OwnerReferences = nil

	ephemeralTwoVolumePod := st.MakePod().Name("abc").Namespace("test").UID("12345II").Volume(v1.Volume{
		Name: "x",
		VolumeSource: v1.VolumeSource{
			Ephemeral: &v1.EphemeralVolumeSource{},
		},
	}).Volume(v1.Volume{
		Name: "y",
		VolumeSource: v1.VolumeSource{
			Ephemeral: &v1.EphemeralVolumeSource{},
		},
	}).Obj()

	ephemeralClaimX := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralTwoVolumePod.Namespace,
			Name:      ephemeralTwoVolumePod.Name + "-" + ephemeralTwoVolumePod.Spec.Volumes[0].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralTwoVolumePod.Name,
					UID:        ephemeralTwoVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	ephemeralClaimY := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralTwoVolumePod.Namespace,
			Name:      ephemeralTwoVolumePod.Name + "-" + ephemeralTwoVolumePod.Spec.Volumes[1].Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Pod",
					Name:       ephemeralTwoVolumePod.Name,
					UID:        ephemeralTwoVolumePod.UID,
					Controller: &controller,
				},
			},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &scName,
		},
	}
	inTreeInlineVolPod := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "aws-inline1",
						},
					},
				},
			},
		},
	}
	inTreeInlineVolPodWithSameCSIVolumeID := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "csi-ebs.csi.aws.com-1",
						},
					},
				},
			},
		},
	}
	onlyConfigmapAndSecretVolPod := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
			},
		},
	}
	pvcPodWithConfigmapAndSecret := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: "csi-ebs.csi.aws.com-0"},
					},
				},
			},
		},
	}
	ephemeralPodWithConfigmapAndSecret := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ephemeralVolumePod.Namespace,
			Name:      ephemeralVolumePod.Name,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					Name: "xyz",
					VolumeSource: v1.VolumeSource{
						Ephemeral: &v1.EphemeralVolumeSource{},
					},
				},
			},
		},
	}
	inlineMigratablePodWithConfigmapAndSecret := &v1.Pod{
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{},
					},
				},
				{
					VolumeSource: v1.VolumeSource{
						AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
							VolumeID: "aws-inline1",
						},
					},
				},
			},
		},
	}
	tests := []struct {
		newPod              *v1.Pod
		existingPods        []*v1.Pod
		extraClaims         []v1.PersistentVolumeClaim
		filterName          string
		maxVols             int
		driverNames         []string
		test                string
		migrationEnabled    bool
		ephemeralEnabled    bool
		limitSource         string
		wantStatus          *framework.Status
		wantPreFilterStatus *framework.Status
	}{
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      4,
			driverNames:  []string{ebsCSIDriverName},
			test:         "fits when node volume limit >= new pods CSI volume",
			limitSource:  "node",
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "doesn't when node volume limit <= pods CSI volume",
			limitSource:  "node",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "should when driver does not support volume limits",
			limitSource:  "csinode-with-no-limit",
		},
		// should count pending PVCs
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{pendingVolumePod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "count pending PVCs towards volume limit <= pods CSI volume",
			limitSource:  "node",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// two same pending PVCs should be counted as 1
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{pendingVolumePod, unboundPVCPod2, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      4,
			driverNames:  []string{ebsCSIDriverName},
			test:         "count multiple pending pvcs towards volume limit >= pods CSI volume",
			limitSource:  "node",
		},
		// should count PVCs with invalid PV name but valid SC
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{missingPVPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "should count PVCs with invalid PV name but valid SC",
			limitSource:  "node",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// don't count a volume which has storageclass missing
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{runningPod, noSCPVCPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName},
			test:         "don't count pvcs with missing SC towards volume limit",
			limitSource:  "node",
		},
		// don't count multiple volume types
		{
			newPod:       csiEBSOneVolPod,
			existingPods: []*v1.Pod{gceTwoVolPod, csiEBSTwoVolPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName, gceCSIDriverName},
			test:         "count pvcs with the same type towards volume limit",
			limitSource:  "node",
			wantStatus:   framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:       gceTwoVolPod,
			existingPods: []*v1.Pod{csiEBSTwoVolPod, runningPod},
			filterName:   "csi",
			maxVols:      2,
			driverNames:  []string{ebsCSIDriverName, gceCSIDriverName},
			test:         "don't count pvcs with different type towards volume limit",
			limitSource:  "node",
		},
		// Tests for in-tree volume migration
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "node",
			test:             "nil csi node",
		},
		{
			newPod:           pendingVolumePod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count unbound in-tree volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode-with-no-limit",
			test:             "should not limit pod if volume used does not report limits",
		},
		{
			newPod:           inTreeNonMigratableOneVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{hostpathInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should not count non-migratable in-tree volumes",
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree inline volumes if migration is enabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// mixed volumes
		{
			newPod:           inTreeOneVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree and csi volumes if migration is enabled (when scheduling in-tree volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPod,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod, inTreeOneVolPod},
			filterName:       "csi",
			maxVols:          3,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree, inline and csi volumes if migration is enabled (when scheduling in-tree volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           inTreeInlineVolPodWithSameCSIVolumeID,
			existingPods:     []*v1.Pod{csiEBSTwoVolPod, inTreeOneVolPod},
			filterName:       "csi",
			maxVols:          3,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should not count in-tree, inline and csi volumes if migration is enabled (when scheduling in-tree volumes)",
		},
		{
			newPod:           csiEBSOneVolPod,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "should count in-tree and csi volumes if migration is enabled (when scheduling csi volumes)",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		// ephemeral volumes
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			driverNames:      []string{ebsCSIDriverName},
			test:             "ephemeral volume missing",
			wantStatus:       framework.AsStatus(errors.New(`looking up PVC test/abc-xyz: persistentvolumeclaim "abc-xyz" not found`)),
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*conflictingClaim},
			driverNames:      []string{ebsCSIDriverName},
			test:             "ephemeral volume not owned",
			wantStatus:       framework.AsStatus(errors.New("PVC test/abc-xyz was not created for pod test/abc (pod is not owner)")),
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			test:             "ephemeral volume unbound",
		},
		{
			newPod:           ephemeralVolumePod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, csiEBSTwoVolPod},
			maxVols:          2,
			limitSource:      "node",
			test:             "ephemeral doesn't when node volume limit <= pods CSI volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaimX, *ephemeralClaimY},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralTwoVolumePod},
			maxVols:          2,
			limitSource:      "node",
			test:             "ephemeral doesn't when node volume limit <= pods ephemeral CSI volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: false,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          3,
			limitSource:      "node",
			test:             "persistent doesn't when node volume limit <= pods ephemeral CSI volume + persistent volume, ephemeral disabled",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          3,
			limitSource:      "node",
			test:             "persistent doesn't when node volume limit <= pods ephemeral CSI volume + persistent volume",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
		{
			newPod:           csiEBSOneVolPod,
			filterName:       "csi",
			ephemeralEnabled: true,
			extraClaims:      []v1.PersistentVolumeClaim{*ephemeralClaim},
			driverNames:      []string{ebsCSIDriverName},
			existingPods:     []*v1.Pod{runningPod, ephemeralVolumePod, csiEBSTwoVolPod},
			maxVols:          4,
			test:             "persistent okay when node volume limit > pods ephemeral CSI volume + persistent volume",
		},
		{
			newPod:              onlyConfigmapAndSecretVolPod,
			filterName:          "csi",
			maxVols:             2,
			driverNames:         []string{ebsCSIDriverName},
			test:                "skip Filter when the pod only uses secrets and configmaps",
			limitSource:         "node",
			wantPreFilterStatus: framework.NewStatus(framework.Skip),
		},
		{
			newPod:      pvcPodWithConfigmapAndSecret,
			filterName:  "csi",
			maxVols:     2,
			driverNames: []string{ebsCSIDriverName},
			test:        "don't skip Filter when the pod has pvcs",
			limitSource: "node",
		},
		{
			newPod:           ephemeralPodWithConfigmapAndSecret,
			filterName:       "csi",
			ephemeralEnabled: true,
			driverNames:      []string{ebsCSIDriverName},
			test:             "don't skip Filter when the pod has ephemeral volumes",
			wantStatus:       framework.AsStatus(errors.New(`looking up PVC test/abc-xyz: persistentvolumeclaim "abc-xyz" not found`)),
		},
		{
			newPod:           inlineMigratablePodWithConfigmapAndSecret,
			existingPods:     []*v1.Pod{inTreeTwoVolPod},
			filterName:       "csi",
			maxVols:          2,
			driverNames:      []string{csilibplugins.AWSEBSInTreePluginName, ebsCSIDriverName},
			migrationEnabled: true,
			limitSource:      "csinode",
			test:             "don't skip Filter when the pod has inline migratable volumes",
			wantStatus:       framework.NewStatus(framework.Unschedulable, ErrReasonMaxVolumeCountExceeded),
		},
	}

	// running attachable predicate tests with feature gate and limit present on nodes
	for _, test := range tests {
		t.Run(test.test, func(t *testing.T) {
			node, csiNode := getNodeWithPodAndVolumeLimits(test.limitSource, test.existingPods, int64(test.maxVols), test.driverNames...)
			if csiNode != nil {
				enableMigrationOnNode(csiNode, csilibplugins.AWSEBSInTreePluginName)
			}
			csiTranslator := csitrans.New()
			p := &CSILimits{
				csiNodeLister:        getFakeCSINodeLister(csiNode),
				pvLister:             getFakeCSIPVLister(test.filterName, test.driverNames...),
				pvcLister:            append(getFakeCSIPVCLister(test.filterName, scName, test.driverNames...), test.extraClaims...),
				scLister:             getFakeCSIStorageClassLister(scName, test.driverNames[0]),
				randomVolumeIDPrefix: rand.String(32),
				translator:           csiTranslator,
			}
			_, ctx := ktesting.NewTestContext(t)
			_, gotPreFilterStatus := p.PreFilter(ctx, nil, test.newPod)
			if diff := cmp.Diff(test.wantPreFilterStatus, gotPreFilterStatus); diff != "" {
				t.Errorf("PreFilter status does not match (-want, +got): %s", diff)
			}
			if gotPreFilterStatus.Code() != framework.Skip {
				gotStatus := p.Filter(ctx, nil, test.newPod, node)
				if !reflect.DeepEqual(gotStatus, test.wantStatus) {
					t.Errorf("Filter status does not match: %v, want: %v", gotStatus, test.wantStatus)
				}
			}
		})
	}
}

func getFakeCSIPVLister(volumeName string, driverNames ...string) tf.PersistentVolumeLister {
	pvLister := tf.PersistentVolumeLister{}
	for _, driver := range driverNames {
		for j := 0; j < 4; j++ {
			volumeHandle := fmt.Sprintf("%s-%s-%d", volumeName, driver, j)
			pv := v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: volumeHandle},
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						CSI: &v1.CSIPersistentVolumeSource{
							Driver:       driver,
							VolumeHandle: volumeHandle,
						},
					},
				},
			}

			switch driver {
			case csilibplugins.AWSEBSInTreePluginName:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
						VolumeID: volumeHandle,
					},
				}
			case hostpathInTreePluginName:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Path: "/tmp",
					},
				}
			default:
				pv.Spec.PersistentVolumeSource = v1.PersistentVolumeSource{
					CSI: &v1.CSIPersistentVolumeSource{
						Driver:       driver,
						VolumeHandle: volumeHandle,
					},
				}
			}
			pvLister = append(pvLister, pv)
		}
	}

	return pvLister
}

func getFakeCSIPVCLister(volumeName, scName string, driverNames ...string) tf.PersistentVolumeClaimLister {
	pvcLister := tf.PersistentVolumeClaimLister{}
	for _, driver := range driverNames {
		for j := 0; j < 4; j++ {
			v := fmt.Sprintf("%s-%s-%d", volumeName, driver, j)
			pvc := v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: v},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: v},
			}
			pvcLister = append(pvcLister, pvc)
		}
	}

	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-4"},
		Spec:       v1.PersistentVolumeClaimSpec{StorageClassName: &scName},
	})
	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-5"},
		Spec:       v1.PersistentVolumeClaimSpec{},
	})
	// a pvc with missing PV but available storageclass.
	pvcLister = append(pvcLister, v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName + "-6"},
		Spec:       v1.PersistentVolumeClaimSpec{StorageClassName: &scName, VolumeName: "missing-in-action"},
	})
	return pvcLister
}

func enableMigrationOnNode(csiNode *storagev1.CSINode, pluginName string) {
	nodeInfoAnnotations := csiNode.GetAnnotations()
	if nodeInfoAnnotations == nil {
		nodeInfoAnnotations = map[string]string{}
	}

	newAnnotationSet := sets.New[string]()
	newAnnotationSet.Insert(pluginName)
	nas := strings.Join(sets.List(newAnnotationSet), ",")
	nodeInfoAnnotations[v1.MigratedPluginsAnnotationKey] = nas

	csiNode.Annotations = nodeInfoAnnotations
}

func getFakeCSIStorageClassLister(scName, provisionerName string) tf.StorageClassLister {
	return tf.StorageClassLister{
		{
			ObjectMeta:  metav1.ObjectMeta{Name: scName},
			Provisioner: provisionerName,
		},
	}
}

func getFakeCSINodeLister(csiNode *storagev1.CSINode) tf.CSINodeLister {
	csiNodeLister := tf.CSINodeLister{}
	if csiNode != nil {
		csiNodeLister = append(csiNodeLister, *csiNode.DeepCopy())
	}
	return csiNodeLister
}

func getNodeWithPodAndVolumeLimits(limitSource string, pods []*v1.Pod, limit int64, driverNames ...string) (*framework.NodeInfo, *storagev1.CSINode) {
	nodeInfo := framework.NewNodeInfo(pods...)
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-for-max-pd-test-1"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{},
		},
	}
	var csiNode *storagev1.CSINode

	addLimitToNode := func() {
		for _, driver := range driverNames {
			node.Status.Allocatable[getVolumeLimitKey(driver)] = *resource.NewQuantity(limit, resource.DecimalSI)
		}
	}

	initCSINode := func() {
		csiNode = &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-for-max-pd-test-1"},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{},
			},
		}
	}

	addDriversCSINode := func(addLimits bool) {
		initCSINode()
		for _, driver := range driverNames {
			driver := storagev1.CSINodeDriver{
				Name:   driver,
				NodeID: "node-for-max-pd-test-1",
			}
			if addLimits {
				driver.Allocatable = &storagev1.VolumeNodeResources{
					Count: pointer.Int32(int32(limit)),
				}
			}
			csiNode.Spec.Drivers = append(csiNode.Spec.Drivers, driver)
		}
	}

	switch limitSource {
	case "node":
		addLimitToNode()
	case "csinode":
		addDriversCSINode(true)
	case "both":
		addLimitToNode()
		addDriversCSINode(true)
	case "csinode-with-no-limit":
		addDriversCSINode(false)
	case "no-csi-driver":
		initCSINode()
	default:
		// Do nothing.
	}

	nodeInfo.SetNode(node)
	return nodeInfo, csiNode
}
