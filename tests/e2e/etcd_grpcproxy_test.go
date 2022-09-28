// Copyright 2017 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/pkg/v3/expect"
	"go.etcd.io/etcd/tests/v3/framework/config"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/framework/testutils"
)

func TestGrpcProxyAutoSync(t *testing.T) {
	e2e.SkipInShortMode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		node1Name      = "node1"
		node1ClientURL = "http://localhost:12379"
		node1PeerURL   = "http://localhost:12380"

		node2Name      = "node2"
		node2ClientURL = "http://localhost:22379"
		node2PeerURL   = "http://localhost:22380"

		proxyClientURL = "127.0.0.1:32379"

		autoSyncInterval = 1 * time.Second
	)

	// Run cluster of one node
	proc1, err := runEtcdNode(
		node1Name, t.TempDir(),
		node1ClientURL, node1PeerURL,
		"new", fmt.Sprintf("%s=%s", node1Name, node1PeerURL),
	)
	require.NoError(t, err)

	// Run grpc-proxy instance
	proxyProc, err := e2e.SpawnCmd([]string{e2e.BinDir + "/etcd", "grpc-proxy", "start",
		"--advertise-client-url", proxyClientURL, "--listen-addr", proxyClientURL,
		"--endpoints", node1ClientURL,
		"--endpoints-auto-sync-interval", autoSyncInterval.String(),
	}, nil)
	require.NoError(t, err)

	proxyCtl := e2e.NewEtcdctl(&e2e.EtcdProcessClusterConfig{}, []string{proxyClientURL})
	err = proxyCtl.Put(ctx, "k1", "v1", config.PutOptions{})
	require.NoError(t, err)

	memberCtl := e2e.NewEtcdctl(&e2e.EtcdProcessClusterConfig{}, []string{node1ClientURL})
	_, err = memberCtl.MemberAdd(ctx, node2Name, []string{node2PeerURL})
	if err != nil {
		t.Fatal(err)
	}

	// Run new member
	proc2, err := runEtcdNode(
		node2Name, t.TempDir(),
		node2ClientURL, node2PeerURL,
		"existing", fmt.Sprintf("%s=%s,%s=%s", node1Name, node1PeerURL, node2Name, node2PeerURL),
	)
	require.NoError(t, err)

	// Wait for auto sync of endpoints
	err = waitForEndpointInLog(proxyProc, node2ClientURL)
	require.NoError(t, err)

	memberList, err := memberCtl.MemberList(ctx)
	require.NoError(t, err)

	node1MemberID, err := findMemberIDByEndpoint(memberList.Members, node1ClientURL)
	require.NoError(t, err)

	// Second node could be not ready yet
	for i := 0; i < 10; i++ {
		_, err = memberCtl.MemberRemove(ctx, node1MemberID)
		if err != nil && strings.Contains(err.Error(), rpctypes.ErrGRPCUnhealthy.Error()) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		break
	}

	// Remove node1 from member list and stop this nod
	require.NoError(t, err)
	require.NoError(t, proc1.Stop())

	var resp *clientv3.GetResponse
	for i := 0; i < 10; i++ {
		resp, err = proxyCtl.Get(ctx, "k1", config.GetOptions{})
		if err != nil && strings.Contains(err.Error(), rpctypes.ErrGRPCLeaderChanged.Error()) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
	}
	require.NoError(t, err)
	kvs := testutils.KeyValuesFromGetResponse(resp)
	assert.Equal(t, []testutils.KV{{Key: "k1", Val: "v1"}}, kvs)

	require.NoError(t, proc2.Stop())
	require.NoError(t, proxyProc.Stop())
}

func runEtcdNode(name, dataDir, clientURL, peerURL, clusterState, initialCluster string) (*expect.ExpectProcess, error) {
	proc, err := e2e.SpawnCmd([]string{e2e.BinDir + "/etcd",
		"--name", name,
		"--data-dir", dataDir,
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster-token", "etcd-cluster",
		"--initial-cluster-state", clusterState,
		"--initial-cluster", initialCluster,
	}, nil)
	if err != nil {
		return nil, err
	}

	_, err = proc.ExpectWithContext(context.Background(), "ready to serve client requests")

	return proc, err
}

func findMemberIDByEndpoint(members []*etcdserverpb.Member, endpoint string) (uint64, error) {
	for _, m := range members {
		if m.ClientURLs[0] == endpoint {
			return m.ID, nil
		}
	}

	return 0, fmt.Errorf("member not found")
}

func waitForEndpointInLog(proxyProc *expect.ExpectProcess, endpoint string) error {
	endpoint = strings.Replace(endpoint, "http://", "", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := proxyProc.ExpectFunc(ctx, func(s string) bool {
		if strings.Contains(s, endpoint) && strings.Contains(s, "Resolver state updated") {
			return true
		}
		return false
	})

	return err
}
