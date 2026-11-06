package controller

import (
	"testing"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnlister "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
)

func setOvnDnatTestObjects(t *testing.T, ctrl *Controller, eip *kubeovnv1.OvnEip, dnat *kubeovnv1.OvnDnatRule) {
	t.Helper()

	eipIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, eipIndexer.Add(eip))
	ctrl.ovnEipsLister = kubeovnlister.NewOvnEipLister(eipIndexer)

	_, err := ctrl.config.KubeOvnClient.KubeovnV1().OvnDnatRules().Create(t.Context(), dnat, metav1.CreateOptions{})
	require.NoError(t, err)
	dnatIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, dnatIndexer.Add(dnat))
	ctrl.ovnDnatRulesLister = kubeovnlister.NewOvnDnatRuleLister(dnatIndexer)
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
		Subnets: []*kubeovnv1.Subnet{subnet},
		IPs:     []*kubeovnv1.IP{oldIP, newIP},
	})
	require.NoError(t, err)
	ctrl := fc.fakeController
	setOvnDnatTestObjects(t, ctrl, eip, dnat)

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

	fc, err := newFakeControllerWithOptions(t, nil)
	require.NoError(t, err)
	ctrl := fc.fakeController
	setOvnDnatTestObjects(t, ctrl, eip, dnat)

	err = ctrl.handleUpdateOvnDnatRule(dnat.Name)
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
