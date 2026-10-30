package controller

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

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
