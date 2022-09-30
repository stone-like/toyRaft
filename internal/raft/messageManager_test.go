package raft

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessageManagerCreateAndParse(t *testing.T) {
	mm := NewMassageManager()

	payload := []byte("hello world")
	messageType := APPEND_ENTRIES

	content, err := mm.Create(messageType, payload)
	require.NoError(t, err)

	gotType, gotPayload, err := mm.Parse(content)
	require.NoError(t, err)

	require.Equal(t, messageType, gotType)
	require.Equal(t, payload, gotPayload)

}
