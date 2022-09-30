package raft

import "encoding/json"

type Payload interface {
	Unmarshal(content []byte) error
	Marshal() ([]byte, error)
}

//後々protocolBufferにするかも
//RPC
//AppendEntiesはハートビートにも使われる
type AppendEntriesRequest struct {
	Term     uint64 //リーダーのterm
	LeaderId string //クライアントからリクエストをfollowerが受け取ったときleaderに流すため

	//ログ追加の時のleaderとflowwerのindexを突き合わせるのに使う,ログがないとき-1が来るのでint64
	PrevLogIndex uint64
	PrevLogTerm  uint64

	Entries      []Log  //ハートビートの時は空
	LeaderCommit uint64 //リーダーのcommitIndex
}

func (a *AppendEntriesRequest) Marshal() ([]byte, error) {
	return json.Marshal(a)
}

func (a *AppendEntriesRequest) Unmarshal(content []byte) error {
	return json.Unmarshal(content, a)

}

type AppendEntriesResponse struct {
	Term    uint64 //currentTerm
	Success bool   //付き合わせが成功したか否か
}

func (a *AppendEntriesResponse) Marshal() ([]byte, error) {
	return json.Marshal(a)
}

func (a *AppendEntriesResponse) Unmarshal(content []byte) error {
	return json.Unmarshal(content, a)

}

// func (a *AppendEntriesResponse) GetTerm() uint64 {
// 	return a.term
// }

type RequestVoteRequest struct {
	Term        uint64 //候補者のterm
	CandidateId uint64 //候補者のId

	//以下は投票の有効性検証のために必要
	LastLogIndex uint64 //候補者の最後のlogのindex
	LastLogterm  uint64 //候補者の最後のlogのterm
}

func (r *RequestVoteRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

func (r *RequestVoteRequest) Unmarshal(content []byte) error {
	return json.Unmarshal(content, r)

}

type RequestVoteResponse struct {
	Term        uint64
	VoteGranted bool //候補者が票を得たか
}

func (r *RequestVoteResponse) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

func (r *RequestVoteResponse) Unmarshal(content []byte) error {
	return json.Unmarshal(content, r)

}

//FollowerにClientMessageきたらリーダーに流す
type ClientMessage struct {
	Content []byte
}

func (c *ClientMessage) Marshal() ([]byte, error) {
	return json.Marshal(c)
}

func (c *ClientMessage) Unmarshal(content []byte) error {
	return json.Unmarshal(content, c)
}

// func (r *RequestVoteResponse) GetTerm() uint64 {
// 	return r.term
// }

// type RPCResponse interface {
// 	GetTerm() uint64
// }
