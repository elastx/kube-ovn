package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnlister "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func TestHandleUpdateOvnSnatRule_DeleteToleratesDeletedVpcRouter(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	snat := &kubeovnv1.OvnSnatRule{
		Name:              "test-snat",
		Finalizers:        []string{util.KubeOVNControllerFinalizer},
		DeletionTimestamp: &now,
		Spec:              kubeovnv1.OvnSnatRuleSpec{OvnEip: "test-eip", Vpc: "test-vpc", V4IpCidr: "10.0.1.0/24"},
	}
	snat.Status = kubeovnv1.OvnSnatRuleStatus{
		Vpc:      "test-vpc",
		V4Eip:    "10.0.0.10",
		V4IpCidr: "10.0.1.0/24",
	}

	fc, err := newFakeControllerWithOptions(t, nil)
	require.NoError(t, err)
	ctrl := fc.fakeController
	_, err = ctrl.config.KubeOvnClient.KubeovnV1().OvnSnatRules().Create(t.Context(), snat, metav1.CreateOptions{})
	require.NoError(t, err)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(snat))
	ctrl.ovnSnatRulesLister = kubeovnlister.NewOvnSnatRuleLister(indexer)
	ctrl.resetOvnEipQueue = newTypedRateLimitingQueue[string]("ResetOvnEip", nil)
	t.Cleanup(ctrl.resetOvnEipQueue.ShutDown)
	ctrl.updateOvnEipQueue = newTypedRateLimitingQueue[string]("UpdateOvnEip", nil)
	t.Cleanup(ctrl.updateOvnEipQueue.ShutDown)

	// the vpc and its logical router were deleted before the snat
	ctrl.OVNNbClient = newInMemoryOVNNbClient(t)

	require.NoError(t, ctrl.handleUpdateOvnSnatRule(snat.Name))

	got, err := ctrl.config.KubeOvnClient.KubeovnV1().OvnSnatRules().Get(t.Context(), snat.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, got.Finalizers, util.KubeOVNControllerFinalizer)
	// the eip waiting for the snat is checked again
	require.Equal(t, 1, ctrl.updateOvnEipQueue.Len())
}
