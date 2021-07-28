package gke

import (
	"context"
	"fmt"
	"sync"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"github.com/blang/semver/v4"
	"github.com/google/uuid"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	containerpb "google.golang.org/genproto/googleapis/container/v1"

	"github.com/kong/kubernetes-testing-framework/pkg/clusters"
)

// Builder generates clusters.Cluster objects backed by GKE given
// provided configuration options.
type Builder struct {
	Name              string
	project, location string
	jsonCreds         []byte

	addons         clusters.Addons
	clusterVersion *semver.Version
	majorMinor     string
}

// NewBuilder provides a new *Builder object.
func NewBuilder(gkeJSONCredentials []byte, project, location string) *Builder {
	return &Builder{
		Name:      fmt.Sprintf("t-%s", uuid.NewString()),
		project:   project,
		location:  location,
		jsonCreds: gkeJSONCredentials,
		addons:    make(clusters.Addons),
	}
}

// WithName indicates a custom name to use for the cluster.
func (b *Builder) WithName(name string) *Builder {
	b.Name = name
	return b
}

// WithClusterVersion configures the Kubernetes cluster version for the Builder
// to use when building the GKE cluster.
func (b *Builder) WithClusterVersion(version semver.Version) *Builder {
	b.clusterVersion = &version
	return b
}

// WithClusterMinorVersion configures the Kubernetes cluster version according
// to a provided Major and Minor version, but will automatically select the latest
// patch version of that minor release (for convenience over the caller having to
// know the entire version tag).
func (b *Builder) WithClusterMinorVersion(major, minor uint64) *Builder {
	b.majorMinor = fmt.Sprintf("%d.%d", major, minor)
	return b
}

// Build creates and configures clients for a GKE-based Kubernetes clusters.Cluster.
func (b *Builder) Build(ctx context.Context) (clusters.Cluster, error) {
	// store the API options with the JSON credentials for auth
	credsOpt := option.WithCredentialsJSON(b.jsonCreds)

	// build the google api client to talk to GKE
	mgrc, err := container.NewClusterManagerClient(ctx, credsOpt)
	if err != nil {
		return nil, err
	}
	defer mgrc.Close()

	// build the google api IAM client to authenticate to the cluster
	gcreds, err := transport.Creds(ctx, credsOpt, option.WithScopes(compute.CloudPlatformScope))
	if err != nil {
		return nil, err
	}
	oauthToken, err := gcreds.TokenSource.Token()
	if err != nil {
		return nil, err
	}

	// configure the cluster creation request
	parent := fmt.Sprintf("projects/%s/locations/%s", b.project, b.location)
	cluster := containerpb.Cluster{
		Name:             b.Name,
		InitialNodeCount: 1,
	}
	req := containerpb.CreateClusterRequest{Parent: parent, Cluster: &cluster}

	// use any provided custom cluster version
	if b.clusterVersion != nil && b.majorMinor != "" {
		return nil, fmt.Errorf("options for full cluster version and partial are mutually exclusive")
	}
	if b.clusterVersion != nil {
		cluster.InitialClusterVersion = b.clusterVersion.String()
	}
	if b.majorMinor != "" {
		latestPatches, err := listLatestClusterPatchVersions(ctx, mgrc, b.project, b.location)
		if err != nil {
			return nil, err
		}
		v, ok := latestPatches[b.majorMinor]
		if !ok {
			return nil, fmt.Errorf("no available kubernetes version for %s", b.majorMinor)
		}
		cluster.InitialClusterVersion = v.String()
	}

	// create the GKE cluster asynchronously
	_, err = mgrc.CreateCluster(ctx, &req)
	if err != nil {
		return nil, err
	}

	// wait for cluster readiness
	clusterReady := false
	for !clusterReady {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("failed to build cluster: %w", err)
			}
			return nil, fmt.Errorf("failed to build cluster: context completed")
		default:
			req := containerpb.GetClusterRequest{Name: fmt.Sprintf("%s/clusters/%s", parent, b.Name)}
			cluster, err := mgrc.GetCluster(ctx, &req)
			if err != nil {
				if _, deleteErr := deleteCluster(ctx, mgrc, b.Name, b.project, b.location); deleteErr != nil {
					return nil, fmt.Errorf("failed to retrieve cluster after building (%s), then failed to clean up: %w", err, deleteErr)
				}
				return nil, err
			}
			if cluster.Status == containerpb.Cluster_RUNNING {
				clusterReady = true
				break
			}
			time.Sleep(waitForClusterTick)
		}
	}

	// get the restconfig and kubernetes client for the cluster
	restCFG, k8s, err := clientForCluster(ctx, mgrc, oauthToken.AccessToken, b.Name, b.project, b.location)
	if err != nil {
		if _, deleteErr := deleteCluster(ctx, mgrc, b.Name, b.project, b.location); deleteErr != nil {
			return nil, fmt.Errorf("failed to get cluster client (%s), then failed to clean up: %w", err, deleteErr)
		}
		return nil, err
	}

	return &gkeCluster{
		name:      b.Name,
		project:   b.project,
		location:  b.location,
		jsonCreds: b.jsonCreds,
		client:    k8s,
		cfg:       restCFG,
		addons:    make(clusters.Addons),
		l:         &sync.RWMutex{},
	}, nil
}