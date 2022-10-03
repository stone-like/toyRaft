package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

//aeのテストを書く
func TestElection(t *testing.T) {
	servers, cleanUp := setupServers(t)

	defer func() {
		cleanUp()
	}()

	require.Eventually(t, func() bool {
		return servers[0].isLeader()
	}, time.Second, 300*time.Millisecond,
	)

	require.True(t, servers[0].isLeader())
}

func TestKeepLeaderAndFollower(t *testing.T) {
	servers, cleanUp := setupServers(t)

	defer func() {
		cleanUp()
	}()

	require.Eventually(t, func() bool {
		return servers[0].isLeader()
	}, time.Second, 300*time.Millisecond,
	)

	time.Sleep(500 * time.Millisecond)
	require.True(t, servers[0].isLeader())
	require.True(t, servers[1].isFollower())
	require.True(t, servers[2].isFollower())
}

//なんでこんなリーダー再選まで時間かかる？<-HeartBeatがやたら多くなってるためだと思う、そのためリクエストチャンネルが圧迫されて遅くなってる
//HeartBeatのピンポンについて調べる
//具体的にはログがないときのHeartBeatぴんぽん
func TestLeaderChange(t *testing.T) {
	servers, cleanUp := setupServers(t)

	defer func() {
		cleanUp()
	}()

	require.Eventually(t, func() bool {
		return servers[0].isLeader()
	}, time.Second, 300*time.Millisecond,
	)

	servers[0].ShutDown()
	require.Eventually(t, func() bool {
		return servers[1].isLeader()
	}, 7*time.Second, 300*time.Millisecond,
	)
	require.True(t, servers[1].isLeader())
	require.True(t, servers[2].isFollower())
}

// func TestLogReplication(t *testing.T) {
// 	servers := setupServers(t)
// }

type CleanUp func()

func setupServers(t *testing.T) ([]*Raft, CleanUp) {
	t.Helper()
	configs := MakeConfig()
	var servers []*Raft
	for _, config := range configs {
		server, err := NewRaft(config)
		require.NoError(t, err)
		servers = append(servers, server)
	}

	cleanUp := func() {
		for _, server := range servers {

			select {
			case <-server.shutdownCh:
				continue
			default:
				server.ShutDown()
			}
		}
	}

	return servers, cleanUp
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
