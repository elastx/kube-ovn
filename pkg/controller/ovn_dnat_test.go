package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnfake "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned/fake"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func TestHandleAddOvnDnatRule_RecordsNatStatusBeforeReady(t *testing.T) {
	t.Parallel()

	eip := &kubeovnv1.OvnEip{
		Name: "test-eip",
		Spec: kubeovnv1.OvnEipSpec{ExternalSubnet: "pubnet"},
	}
	eip.Status.V4Ip = "10.0.0.10"
	dnat := &kubeovnv1.OvnDnatRule{
		Name: "test-dnat",
		Spec: kubeovnv1.OvnDnatRuleSpec{
			OvnEip:       eip.Name,
			Vpc:          "test-vpc",
			V4Ip:         "10.0.1.5",
			Protocol:     "tcp",
			ExternalPort: "2222",
			InternalPort: "22",
		},
	}

	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		OvnEips:      []*kubeovnv1.OvnEip{eip},
		OvnDnatRules: []*kubeovnv1.OvnDnatRule{dnat},
	})
	require.NoError(t, err)
	ctrl := fc.fakeController

	fc.mockOvnClient.EXPECT().CreateLoadBalancer(dnat.Name, dnat.Spec.Protocol).Return(nil)
	fc.mockOvnClient.EXPECT().LoadBalancerAddVip(dnat.Name, eip.Status.V4Ip+":"+dnat.Spec.ExternalPort, dnat.Spec.V4Ip+":"+dnat.Spec.InternalPort).Return(nil)
	fc.mockOvnClient.EXPECT().LogicalRouterUpdateLoadBalancers(dnat.Spec.Vpc, ovsdb.MutateOperationInsert, dnat.Name).Return(nil)

	kubeovnClient, ok := ctrl.config.KubeOvnClient.(*kubeovnfake.Clientset)
	require.True(t, ok)
	kubeovnClient.PrependReactor("patch", "ovn-dnat-rules", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if ok && patchAction.GetPatchType() == types.JSONPatchType &&
			strings.Contains(string(patchAction.GetPatch()), "/metadata/annotations") {
			return true, nil, errors.New("injected failure after nat status recorded")
		}
		return false, nil, nil
	})

	err = ctrl.handleAddOvnDnatRule(dnat.Name)
	require.Error(t, err)

	got, err := ctrl.config.KubeOvnClient.KubeovnV1().OvnDnatRules().Get(t.Context(), dnat.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, got.Finalizers, util.KubeOVNControllerFinalizer)
	require.Equal(t, dnat.Spec.Vpc, got.Status.Vpc)
	require.Equal(t, eip.Status.V4Ip, got.Status.V4Eip)
	require.Equal(t, dnat.Spec.ExternalPort, got.Status.ExternalPort)
	require.False(t, got.Status.Ready)
}

func TestHandleUpdateOvnDnatRule_DeleteRemovesLBAndFinalizer(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	dnat := &kubeovnv1.OvnDnatRule{
		Name:              "test-dnat",
		Finalizers:        []string{util.KubeOVNControllerFinalizer},
		DeletionTimestamp: &now,
		Spec:              kubeovnv1.OvnDnatRuleSpec{OvnEip: "test-eip", Vpc: "test-vpc"},
	}
	dnat.Status = kubeovnv1.OvnDnatRuleStatus{
		Vpc:          "test-vpc",
		V4Eip:        "10.0.0.10",
		ExternalPort: "2222",
	}

	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{OvnDnatRules: []*kubeovnv1.OvnDnatRule{dnat}})
	require.NoError(t, err)
	ctrl := fc.fakeController
	ctrl.resetOvnEipQueue = newTypedRateLimitingQueue[string]("ResetOvnEip", nil)
	t.Cleanup(ctrl.resetOvnEipQueue.ShutDown)
	ctrl.updateOvnEipQueue = newTypedRateLimitingQueue[string]("UpdateOvnEip", nil)
	t.Cleanup(ctrl.updateOvnEipQueue.ShutDown)

	fc.mockOvnClient.EXPECT().
		LoadBalancerDeleteVip(dnat.Name, dnat.Status.V4Eip+":"+dnat.Status.ExternalPort, true).
		Return(nil)
	fc.mockOvnClient.EXPECT().
		LogicalRouterUpdateLoadBalancers(dnat.Status.Vpc, ovsdb.MutateOperationDelete, dnat.Name).
		Return(nil)

	require.NoError(t, ctrl.handleUpdateOvnDnatRule(dnat.Name))

	got, err := ctrl.config.KubeOvnClient.KubeovnV1().OvnDnatRules().Get(t.Context(), dnat.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, got.Finalizers, util.KubeOVNControllerFinalizer)
}

func TestHandleUpdateOvnDnatRule_InternalIPChangeUpdatesTarget(t *testing.T) {
	t.Parallel()

	eip := &kubeovnv1.OvnEip{
		Name: "test-eip",
		Spec: kubeovnv1.OvnEipSpec{ExternalSubnet: "pubnet"},
	}
	eip.Status.V4Ip = "10.0.0.10"
	subnet := &kubeovnv1.Subnet{
		Name: "test-subnet",
		Spec: kubeovnv1.SubnetSpec{Vpc: "test-vpc", CIDRBlock: "10.0.1.0/24", Protocol: kubeovnv1.ProtocolIPv4},
	}
	oldIP := &kubeovnv1.IP{Name: "old-ip", Spec: kubeovnv1.IPSpec{Subnet: subnet.Name, V4IPAddress: "10.0.1.2"}}
	newIP := &kubeovnv1.IP{Name: "new-ip", Spec: kubeovnv1.IPSpec{Subnet: subnet.Name, V4IPAddress: "10.0.1.3"}}
	dnat := &kubeovnv1.OvnDnatRule{
		Name: "test-dnat",
		Spec: kubeovnv1.OvnDnatRuleSpec{
			OvnEip:       eip.Name,
			IPName:       newIP.Name,
			Protocol:     "tcp",
			ExternalPort: "2222",
			InternalPort: "22",
		},
	}
	dnat.Status = kubeovnv1.OvnDnatRuleStatus{
		Ready:        true,
		Vpc:          subnet.Spec.Vpc,
		V4Eip:        eip.Status.V4Ip,
		V4Ip:         oldIP.Spec.V4IPAddress,
		IPName:       oldIP.Name,
		Protocol:     "tcp",
		ExternalPort: "2222",
		InternalPort: "22",
	}

	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		OvnEips:      []*kubeovnv1.OvnEip{eip},
		Subnets:      []*kubeovnv1.Subnet{subnet},
		IPs:          []*kubeovnv1.IP{oldIP, newIP},
		OvnDnatRules: []*kubeovnv1.OvnDnatRule{dnat},
	})
	require.NoError(t, err)
	ctrl := fc.fakeController

	fc.mockOvnClient.EXPECT().CreateLoadBalancer(dnat.Name, dnat.Spec.Protocol).Return(nil)
	fc.mockOvnClient.EXPECT().LoadBalancerAddVip(dnat.Name, eip.Status.V4Ip+":"+dnat.Spec.ExternalPort, newIP.Spec.V4IPAddress+":"+dnat.Spec.InternalPort).Return(nil)
	fc.mockOvnClient.EXPECT().LogicalRouterUpdateLoadBalancers(subnet.Spec.Vpc, ovsdb.MutateOperationInsert, dnat.Name).Return(nil)

	require.NoError(t, ctrl.handleUpdateOvnDnatRule(dnat.Name))

	got, err := ctrl.config.KubeOvnClient.KubeovnV1().OvnDnatRules().Get(t.Context(), dnat.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, got.Status.Ready)
	require.Equal(t, newIP.Name, got.Status.IPName)
	require.Equal(t, newIP.Spec.V4IPAddress, got.Status.V4Ip)
}

func TestHandleUpdateOvnDnatRule_InternalIPFamilyChangeIsRejected(t *testing.T) {
	t.Parallel()

	eip := &kubeovnv1.OvnEip{
		Name: "test-eip",
		Spec: kubeovnv1.OvnEipSpec{ExternalSubnet: "pubnet"},
	}
	eip.Status.V4Ip = "10.0.0.10"
	eip.Status.V6Ip = "fd00::10"
	dnat := &kubeovnv1.OvnDnatRule{
		Name: "test-dnat",
		Spec: kubeovnv1.OvnDnatRuleSpec{
			OvnEip:       eip.Name,
			Vpc:          "test-vpc",
			V6Ip:         "fd00:1::3",
			Protocol:     "tcp",
			ExternalPort: "2222",
			InternalPort: "22",
		},
	}
	dnat.Status = kubeovnv1.OvnDnatRuleStatus{
		Ready:        true,
		Vpc:          "test-vpc",
		V4Eip:        eip.Status.V4Ip,
		V6Eip:        eip.Status.V6Ip,
		V4Ip:         "10.0.1.2",
		Protocol:     "tcp",
		ExternalPort: "2222",
		InternalPort: "22",
	}

	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		OvnEips:      []*kubeovnv1.OvnEip{eip},
		OvnDnatRules: []*kubeovnv1.OvnDnatRule{dnat},
	})
	require.NoError(t, err)

	err = fc.fakeController.handleUpdateOvnDnatRule(dnat.Name)
	require.ErrorContains(t, err, "not support change ip family of internal ip")
}

func TestEnqueueUpdateOvnDnatRule_InternalIPChange(t *testing.T) {
	t.Parallel()

	ctrl := &Controller{
		updateOvnDnatRuleQueue: newTypedRateLimitingQueue[string]("UpdateOvnDnatRule", nil),
	}
	t.Cleanup(ctrl.updateOvnDnatRuleQueue.ShutDown)

	oldDnat := &kubeovnv1.OvnDnatRule{
		Name: "test-dnat",
		Spec: kubeovnv1.OvnDnatRuleSpec{OvnEip: "test-eip", V4Ip: "10.0.1.2"},
	}
	newDnat := oldDnat.DeepCopy()
	newDnat.Spec.V4Ip = "10.0.1.3"

	ctrl.enqueueUpdateOvnDnatRule(oldDnat, newDnat)
	require.Equal(t, 1, ctrl.updateOvnDnatRuleQueue.Len())
}
