package controller

import (
	"errors"
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnfake "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned/fake"
	kubeovnlister "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

// Keep the lister snapshot frozen while API writes advance, as can happen before
// the informer observes a concurrent external gateway or VPC reconciliation.
func freezeVpcLister(t *testing.T, ctrl *Controller, vpc *kubeovnv1.Vpc) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(vpc.DeepCopy()))
	ctrl.vpcsLister = kubeovnlister.NewVpcLister(indexer)
}

func TestUpdateDefaultVpcExternalPreservesConcurrentStatus(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			t.Parallel()
			cachedVpc := &kubeovnv1.Vpc{
				Name: util.DefaultVpc,
				Spec: kubeovnv1.VpcSpec{EnableExternal: !enabled},
				Status: kubeovnv1.VpcStatus{
					EnableExternal: !enabled,
					Subnets:        []string{"old-subnet"},
				},
			}
			liveVpc := cachedVpc.DeepCopy()
			liveVpc.Spec.Namespaces = []string{"new-namespace"}
			liveVpc.Status.Subnets = []string{"new-subnet"}
			liveVpc.Status.TCPLoadBalancer = "new-load-balancer"
			liveVpc.Status.BFDPort.Name = "new-bfd-port"
			ctrl := &Controller{config: &Configuration{
				ClusterRouter: util.DefaultVpc,
				KubeOvnClient: kubeovnfake.NewSimpleClientset(liveVpc),
			}}
			freezeVpcLister(t, ctrl, cachedVpc)

			require.NoError(t, ctrl.updateDefaultVpcExternal(enabled))

			got, err := ctrl.config.KubeOvnClient.KubeovnV1().Vpcs().Get(t.Context(), util.DefaultVpc, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, enabled, got.Spec.EnableExternal)
			require.Equal(t, enabled, got.Status.EnableExternal)
			liveVpc.Status.EnableExternal = enabled
			require.Equal(t, liveVpc.Status, got.Status)
			liveVpc.Spec.EnableExternal = enabled
			require.Equal(t, liveVpc.Spec, got.Spec)
		})
	}
}

func TestResyncExternalGatewayRepairsVpcStatus(t *testing.T) {
	oldEnabled, oldConfig := exGwEnabled, lastExGwCM
	t.Cleanup(func() { exGwEnabled, lastExGwCM = oldEnabled, oldConfig })

	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			vpc := &kubeovnv1.Vpc{
				Name:   util.DefaultVpc,
				Spec:   kubeovnv1.VpcSpec{EnableExternal: enabled},
				Status: kubeovnv1.VpcStatus{EnableExternal: !enabled},
			}
			cm := &corev1.ConfigMap{
				Name:      util.ExternalGatewayConfig,
				Namespace: metav1.NamespaceSystem,
				Data:      map[string]string{"enable-external-gw": strconv.FormatBool(enabled)},
			}
			opts := &FakeControllerOptions{Vpcs: []*kubeovnv1.Vpc{vpc}}
			if enabled {
				opts.ConfigMaps = []*corev1.ConfigMap{cm}
			}
			fc, err := newFakeControllerWithOptions(t, opts)
			require.NoError(t, err)
			ctrl := fc.fakeController
			ctrl.config.ExternalGatewayConfigNS = cm.Namespace
			freezeVpcLister(t, ctrl, vpc)
			exGwEnabled, lastExGwCM = strconv.FormatBool(enabled), cm.Data
			attempts := 0
			client := ctrl.config.KubeOvnClient.(*kubeovnfake.Clientset)
			client.PrependReactor("patch", "vpcs", func(action ktesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "status" {
					attempts++
					if attempts == 1 {
						return true, nil, errors.New("temporary status update failure")
					}
				}
				return false, nil, nil
			})

			// No OVN operations are expected when only the VPC status has drifted.
			ctrl.resyncExternalGateway()
			require.Equal(t, 1, attempts)
			ctrl.resyncExternalGateway()
			require.Equal(t, 2, attempts)

			got, err := ctrl.config.KubeOvnClient.KubeovnV1().Vpcs().Get(t.Context(), vpc.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, enabled, got.Spec.EnableExternal)
			require.Equal(t, enabled, got.Status.EnableExternal)
		})
	}
}

func newExternalGatewayTestController(t *testing.T, exGwNodes string) *fakeController {
	t.Helper()

	// resyncExternalGateway keeps its state in package variables
	oldEnabled, oldCM := exGwEnabled, lastExGwCM
	t.Cleanup(func() { exGwEnabled, lastExGwCM = oldEnabled, oldCM })

	nodes := []*corev1.Node{
		{Name: "node-1", Annotations: map[string]string{util.ChassisAnnotation: "chassis-1"}},
		{Name: "node-2", Annotations: map[string]string{util.ChassisAnnotation: "chassis-2"}},
		{Name: "node-3"},
	}
	cm := &corev1.ConfigMap{
		Name:      util.ExternalGatewayConfig,
		Namespace: metav1.NamespaceSystem,
		Data:      externalGatewayConfig(exGwNodes),
	}
	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		Vpcs:       []*kubeovnv1.Vpc{{Name: util.DefaultVpc}},
		Nodes:      nodes,
		ConfigMaps: []*corev1.ConfigMap{cm},
	})
	require.NoError(t, err)
	fc.fakeController.config.ExternalGatewayConfigNS = metav1.NamespaceSystem
	fc.fakeController.config.ExternalGatewaySwitch = util.ExternalGatewaySwitch
	fc.fakeController.config.ExternalGatewayNet = "external"
	return fc
}

func externalGatewayConfig(exGwNodes string) map[string]string {
	return map[string]string{
		"enable-external-gw": "true",
		"type":               kubeovnv1.GWCentralizedType,
		"external-gw-nodes":  exGwNodes,
		"nic-ip":             "172.56.0.2/16",
		"nic-mac":            "00:00:00:12:34:56",
	}
}

func updateExternalGatewayConfig(t *testing.T, fc *fakeController, exGwNodes string) {
	t.Helper()

	c := fc.fakeController
	cms := c.config.KubeClient.CoreV1().ConfigMaps(metav1.NamespaceSystem)
	cm, err := cms.Get(t.Context(), util.ExternalGatewayConfig, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data = externalGatewayConfig(exGwNodes)
	_, err = cms.Update(t.Context(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		cached, err := c.configMapsLister.ConfigMaps(metav1.NamespaceSystem).Get(util.ExternalGatewayConfig)
		return err == nil && maps.Equal(cached.Data, cm.Data)
	}, 5*time.Second, 10*time.Millisecond)
}

func expectGetChassis(fc *fakeController) {
	fc.mockOvnSbClient.EXPECT().GetChassis(gomock.Any(), false).DoAndReturn(
		func(name string, _ bool) (*ovnsb.Chassis, error) {
			return &ovnsb.Chassis{Name: name}, nil
		},
	).AnyTimes()
}

func TestResyncExternalGatewayRetriesAfterFailedEstablish(t *testing.T) {
	fc := newExternalGatewayTestController(t, "node-1,node-3")
	c := fc.fakeController
	expectGetChassis(fc)

	// the gateway was established on node-1 and node-2, then node-2 was
	// replaced by node-3 which is not ready yet
	exGwEnabled = "true"
	lastExGwCM = externalGatewayConfig("node-1,node-2")

	lrpName := util.DefaultVpc + "-" + util.ExternalGatewaySwitch
	fc.mockOvnClient.EXPECT().DeleteLogicalGatewaySwitch(util.ExternalGatewaySwitch, util.DefaultVpc).Return(nil)
	c.resyncExternalGateway()

	// node-3 is replaced by node-2 again
	updateExternalGatewayConfig(t, fc, "node-1,node-2")
	fc.mockOvnClient.EXPECT().DeleteLogicalGatewaySwitch(util.ExternalGatewaySwitch, util.DefaultVpc).Return(nil)
	fc.mockOvnClient.EXPECT().GetLogicalRouterPort(lrpName, true).Return(nil, nil)
	fc.mockOvnClient.EXPECT().CreateGatewayLogicalSwitch(util.ExternalGatewaySwitch, util.DefaultVpc, "external",
		"172.56.0.2/16", "00:00:00:12:34:56", 0, "chassis-1", "chassis-2").Return(nil)
	c.resyncExternalGateway()

	require.Equal(t, "true", exGwEnabled)
	require.Equal(t, externalGatewayConfig("node-1,node-2"), lastExGwCM)
}

func TestResyncExternalGatewayRetriesAfterFailedRemove(t *testing.T) {
	fc := newExternalGatewayTestController(t, "node-1,node-3")
	c := fc.fakeController
	expectGetChassis(fc)

	exGwEnabled = "true"
	lastExGwCM = externalGatewayConfig("node-1,node-2")

	lrpName := util.DefaultVpc + "-" + util.ExternalGatewaySwitch
	fc.mockOvnClient.EXPECT().DeleteLogicalGatewaySwitch(util.ExternalGatewaySwitch, util.DefaultVpc).Return(errors.New("nb unavailable"))
	c.resyncExternalGateway()

	// the gateway nodes have been unlabeled, so the old config must be
	// established again
	updateExternalGatewayConfig(t, fc, "node-1,node-2")
	fc.mockOvnClient.EXPECT().GetLogicalRouterPort(lrpName, true).Return(nil, nil)
	fc.mockOvnClient.EXPECT().CreateGatewayLogicalSwitch(util.ExternalGatewaySwitch, util.DefaultVpc, "external",
		"172.56.0.2/16", "00:00:00:12:34:56", 0, "chassis-1", "chassis-2").Return(nil)
	c.resyncExternalGateway()

	require.Equal(t, "true", exGwEnabled)
}

func TestGetGatewayChassisDoesNotLabelNodesOnFailure(t *testing.T) {
	fc := newExternalGatewayTestController(t, "node-1,node-3")
	c := fc.fakeController
	expectGetChassis(fc)

	_, err := c.getGatewayChassis(externalGatewayConfig("node-1,node-3"))
	require.ErrorContains(t, err, "node node-3 has no chassis annotation")

	for _, name := range []string{"node-1", "node-3"} {
		node, err := c.config.KubeClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotContains(t, node.Labels, util.ExGatewayLabel, "node %s", name)
	}
}

func TestGetGatewayChassisLabelsNodes(t *testing.T) {
	fc := newExternalGatewayTestController(t, "node-1,node-2")
	c := fc.fakeController
	expectGetChassis(fc)

	chassises, err := c.getGatewayChassis(externalGatewayConfig("node-1,node-2"))
	require.NoError(t, err)
	require.Equal(t, []string{"chassis-1", "chassis-2"}, chassises)

	for _, name := range []string{"node-1", "node-2"} {
		node, err := c.config.KubeClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "true", node.Labels[util.ExGatewayLabel], "node %s", name)
	}
}
