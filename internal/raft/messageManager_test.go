package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageManagerCreateAndParse(t *testing.T) {
	mm := NewMassageManager()

	payload := []byte("hello world")
	messageType := APPEND_ENTRIES
	addr := "192.0.0.0.1"

	content, err := mm.Create(messageType, addr, payload)
	require.NoError(t, err)

	msg, err := mm.Parse(content)
	require.NoError(t, err)

	require.Equal(t, messageType, msg.MessageType)
	require.Equal(t, addr, msg.Addr)
	require.Equal(t, payload, msg.Payload)

}
