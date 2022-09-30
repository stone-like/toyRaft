package raft

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
)

func TestNodeIndex(t *testing.T) {
	c1 := &Config{
		serverAddr: "192.0.0.1:8080",
		peerAddrs: []string{
			"192.0.0.1:8081",
			"192.0.0.1:8082",
		},
	}

	c2 := &Config{
		serverAddr: "192.0.0.1:8081",
		peerAddrs: []string{
			"192.0.0.1:8080",
			"192.0.0.1:8082",
		},
	}

	nodes1 := makeNodes(c1)
	nodes2 := makeNodes(c2)

	require.Equal(t, nodes1[0].ServerId, int64(0))
	require.Equal(t, nodes1[0].Addr, "192.0.0.1:8080")

	require.Equal(t, nodes1[1].ServerId, int64(1))
	require.Equal(t, nodes1[1].Addr, "192.0.0.1:8081")

	require.Equal(t, nodes1[2].ServerId, int64(2))
	require.Equal(t, nodes1[2].Addr, "192.0.0.1:8082")

	if diff := cmp.Diff(nodes1, nodes2); diff != "" {
		t.Errorf(diff)
	}

}
