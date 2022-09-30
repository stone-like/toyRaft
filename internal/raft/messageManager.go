package raft

import "encoding/json"

type MessageType int

const (
	APPEND_ENTRIES MessageType = iota + 1
	REQUEST_VOTE
	APPEND_ENTRIES_RESPONSE
	REQUEST_VOTE_RESPONSE
)

type Message struct {
	MessageType MessageType
	Payload     []byte
}

type MessageManager struct{}

func NewMassageManager() *MessageManager {
	return &MessageManager{}
}

func (mm *MessageManager) Create(messageType MessageType, payload []byte) ([]byte, error) {
	m := &Message{
		MessageType: messageType,
		Payload:     payload,
	}

	return json.Marshal(m)

}

func (mm *MessageManager) Parse(content []byte) (MessageType, []byte, error) {
	var msg Message
	if err := json.Unmarshal(content, &msg); err != nil {
		return 0, nil, err
	}

	return msg.MessageType, msg.Payload, nil
}

func (mm *MessageManager) CreateAppendEntriesRequest(term, prevLogIndex, prevLogTerm, leaderCommit uint64, entries []Log, leaderId string) *AppendEntriesRequest {
	return &AppendEntriesRequest{
		Term:         term,
		LeaderId:     leaderId,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: leaderCommit,
	}
}

func (mm *MessageManager) CreateRequestVoteRequest() *RequestVoteRequest {
	return &RequestVoteRequest{}
}

func (mm *MessageManager) CreateAppendEntriesResponse(term uint64, success bool) *AppendEntriesResponse {
	return &AppendEntriesResponse{
		Term:    term,
		Success: success,
	}
}

func (mm *MessageManager) CreateRequestVoteResponse(term uint64, voteGranted bool) *RequestVoteResponse {
	return &RequestVoteResponse{
		Term:        term,
		VoteGranted: voteGranted,
	}
}
