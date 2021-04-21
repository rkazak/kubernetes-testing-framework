//+build integration_tests

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	apix "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kong/kubernetes-testing-framework/pkg/kind"
)

func TestKongProxyClusterWithMetalLB(t *testing.T) {
	config := kind.ClusterConfigurationWithKongProxy{
		DockerNetwork: kind.DefaultKindDockerNetwork,
		EnableMetalLB: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*10)
	defer cancel()

	cluster, ready, err := config.Deploy(ctx)
	assert.NoError(t, err)
	defer cluster.Cleanup()

	event := <-ready
	assert.NoError(t, event.Err)
	assert.NotEmpty(t, event)

	assert.Eventually(t, func() bool {
		resp, err := http.Get(event.ProxyURL.String())
		if err != nil {
			t.Logf("received error while trying to reach the proxy at %s: %v", event.ProxyURL, err)
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Logf("expected status code %d, received: %d", http.StatusNotFound, resp.StatusCode)
			return false
		}
		return resp.StatusCode == http.StatusNotFound
	}, time.Minute*3, time.Millisecond*200)

	// the proxy-only configuration should have no pre-installed CRDs present
	c, err := apix.NewForConfig(cluster.Config())
	assert.NoError(t, err)
	crds, err := c.CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Len(t, crds.Items, 0)
}
