package raft

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
)

func TestMessageMarshalAndUnmarshal(t *testing.T) {
	mm := NewMassageManager()

	message1 := &AppendEntriesRequest{
		0,
		"1",
		2,
		3,
		[]Log{},
		4,
	}
	payload, err := message1.Marshal()
	require.NoError(t, err)

	messageType := APPEND_ENTRIES

	content, err := mm.Create(messageType, payload)
	require.NoError(t, err)

	gotType, gotPayload, err := mm.Parse(content)
	require.NoError(t, err)

	require.Equal(t, messageType, gotType)

	gotMessage1 := &AppendEntriesRequest{}
	err = gotMessage1.Unmarshal(gotPayload)
	require.NoError(t, err)

	if diff := cmp.Diff(message1, gotMessage1); diff != "" {
		t.Errorf("diff occured:\n%s", diff)
	}

	//todo tableDriveで書く
	message2 := &RequestVoteRequest{
		0,
		1,
		2,
		3,
	}
	payload, err = message2.Marshal()
	require.NoError(t, err)

	messageType = REQUEST_VOTE

	content, err = mm.Create(messageType, payload)
	require.NoError(t, err)

	gotType, gotPayload, err = mm.Parse(content)
	require.NoError(t, err)

	require.Equal(t, messageType, gotType)

	gotMessag2 := &RequestVoteRequest{}
	err = gotMessag2.Unmarshal(gotPayload)
	require.NoError(t, err)

	if diff := cmp.Diff(message2, gotMessag2); diff != "" {
		t.Errorf("diff occured:\n%s", diff)
	}

}
