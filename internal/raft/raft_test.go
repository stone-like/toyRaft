package raft

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/travisjeffery/go-dynaport"
)

//aeのテストを書く
func TestElection(t *testing.T) {
	servers, _, cleanUp := setupServers(t)

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
	servers, _, cleanUp := setupServers(t)

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

func TestLeaderChange(t *testing.T) {
	servers, _, cleanUp := setupServers(t)

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

func TestLogReplication(t *testing.T) {
	servers, fsms, cleanUp := setupServers(t)

	defer func() {
		cleanUp()
	}()

	checkFSM := func(serverId int) bool {
		for _, fsm := range fsms {
			if fsm.ServerId != serverId {
				continue
			}

			return len(fsm.store.store) == 1
		}

		return false
	}

	condition := func() bool {
		leaderLogOk := len(servers[0].state.logs) == 1 && string(servers[0].state.logs[0].Content) == "hello world"
		follower1LogOk := len(servers[1].state.logs) == 1 && string(servers[1].state.logs[0].Content) == "hello world"
		follower2LogOk := len(servers[2].state.logs) == 1 && string(servers[2].state.logs[0].Content) == "hello world"
		nextIndexOK := checkNextIndexForNode(servers[0], servers[1].addr, servers[1].serverId, 1) &&
			checkNextIndexForNode(servers[0], servers[2].addr, servers[2].serverId, 1)
		matchIndexOK := checkMatchIndexForNode(servers[0], servers[1].addr, servers[1].serverId, 0) &&
			checkMatchIndexForNode(servers[0], servers[2].addr, servers[2].serverId, 0)
		commitIndexOK := servers[0].state.commitIndex == 0
		fsmIsOK := checkFSM(servers[0].serverId) && checkFSM(servers[1].serverId) && checkFSM(servers[0].serverId)

		return leaderLogOk &&
			follower1LogOk &&
			follower2LogOk &&
			nextIndexOK &&
			matchIndexOK &&
			commitIndexOK &&
			fsmIsOK
	}

	require.Eventually(t, func() bool {
		return servers[0].isLeader()
	}, time.Second, 300*time.Millisecond,
	)
	sendClientMessage(t, servers[0])

	require.Eventually(t, condition, 5*time.Second, 300*time.Millisecond)

}

func checkNextIndexForNode(server *Raft, nodeAddr string, nodeId int, index int) bool {
	node := Node{Addr: nodeAddr, ServerId: nodeId}
	nextIndex := server.state.nextIndexes[node]

	return nextIndex == index

}

func checkMatchIndexForNode(server *Raft, nodeAddr string, nodeId int, index int) bool {
	node := Node{Addr: nodeAddr, ServerId: nodeId}
	matchIndex := server.state.matchIndexes[node]

	return matchIndex == index

}

func sendClientMessage(t *testing.T, server *Raft) {
	conn, err := net.Dial("tcp", server.addr)
	require.NoError(t, err)
	defer conn.Close()

	clientMessage := ClientMessage{
		[]byte("hello world"),
	}

	payload, err := clientMessage.Marshal()
	require.NoError(t, err)
	m := &Message{
		MessageType: CLIENT_MESSAGE,
		Addr:        "dummy",
		Payload:     payload,
	}

	msg, err := json.Marshal(m)
	require.NoError(t, err)

	conn.Write(msg)
}

type CleanUp func()

func setupServers(t *testing.T) ([]*Raft, []FakeFSM, CleanUp) {
	t.Helper()
	configs := MakeConfig()
	var servers []*Raft
	var FSMs []FakeFSM
	for _, config := range configs {
		fsm := &Faker{}
		config.fsm = fsm
		server, err := NewRaft(config)
		require.NoError(t, err)

		servers = append(servers, server)
		FSMs = append(FSMs, FakeFSM{store: fsm, ServerId: server.serverId})
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

	return servers, FSMs, cleanUp
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

type Faker struct {
	store []string
	mu    sync.Mutex
}

func (f *Faker) Apply(payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.store = append(f.store, string(payload))

	return nil
}

type FakeFSM struct {
	store    *Faker
	ServerId int
}

func MakeConfig() []*Config {

	makePorts := func() []int {
		return dynaport.Get(3)
	}

	funcs := []func() time.Duration{
		func() time.Duration { return 150 * time.Millisecond },
		func() time.Duration { return 220 * time.Millisecond },
		func() time.Duration { return 300 * time.Millisecond },
	}

	makeServers := func(ports []int, fns []func() time.Duration) []Server {
		var servers []Server
		for i := 0; i < len(ports); i++ {
			servers = append(servers, Server{
				addr:                 fmt.Sprintf("127.0.0.1:%d", ports[i]),
				generateElectionTime: fns[i],
			})
		}

		return servers
	}

	servers := makeServers(makePorts(), funcs)

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
