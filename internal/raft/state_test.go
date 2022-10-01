package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogIndex(t *testing.T) {
	logs := Logs{
		Log{Term: 1}, Log{Term: 2}, Log{Term: 3},
	}

	state1 := &State{logs: logs}

	nilLogs := Logs{}
	state2 := &State{logs: nilLogs}

	_, exists := state1.getLog(3)
	require.False(t, exists)
	_, exists = state1.getLog(2)
	require.True(t, exists)

	ret := state1.getSendableLogs(1)
	require.Equal(t, len(ret), 2)
	ret = state1.getSendableLogs(3)
	require.Nil(t, ret)
	ret = state2.getSendableLogs(0)
	require.Nil(t, ret)

	state1.deleteLogs(1)
	require.Equal(t, len(state1.logs), 1)

	term := state2.getLogTerm(3)
	require.Equal(t, uint64(0), term)
	term = state1.getLogTerm(3)
	require.Equal(t, uint64(0), term)
	term = state1.getLogTerm(0)
	require.Equal(t, uint64(1), term)

}
