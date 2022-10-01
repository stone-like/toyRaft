package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

//aeのテストを書く
func TestElection(t *testing.T) {
	servers := setupServers(t)
	println(servers)
	require.Eventually(t, func() bool {
		return servers[0].isLeader()
	}, time.Second, 500*time.Millisecond,
	)

	require.True(t, servers[0].isLeader())
}

// func getLeader(servers []*Raft) (*Raft, bool) {

// 	for _, server := range servers {
// 		if server.isLeader() {
// 			return server, true
// 		}
// 	}
// 	return nil, false
// }

func setupServers(t *testing.T) []*Raft {
	t.Helper()
	configs := MakeConfig()
	var servers []*Raft
	for _, config := range configs {
		server, err := NewRaft(config)
		require.NoError(t, err)
		servers = append(servers, server)
	}

	return servers
}

func Copy(s []Server) []Server {
	temp := make([]Server, len(s))

	for i, v := range s {
		temp[i] = v
	}

	return temp
}

type Server struct {
	addr                 string
	generateElectionTime func() time.Duration
}

func MakeConfig() []*Config {
	servers := []Server{
		{"127.0.0.1:8080", func() time.Duration { return 150 * time.Millisecond }},
		{"127.0.0.1:8081", func() time.Duration { return 220 * time.Millisecond }},
		{"127.0.0.1:8082", func() time.Duration { return 300 * time.Millisecond }},
	}

	var configs []*Config

	for i, server := range servers {

		peers := Copy(servers[:i])
		peers = append(peers, servers[i+1:]...)

		var peerAddrs []string
		for _, peer := range peers {
			peerAddrs = append(peerAddrs, peer.addr)
		}
		config := &Config{
			serverAddr:             server.addr,
			peerAddrs:              peerAddrs,
			generateElectionTimeFn: server.generateElectionTime,
		}
		configs = append(configs, config)
	}

	return configs

}
