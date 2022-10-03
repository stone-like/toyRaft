package raft

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"
)

var (
	ErrUnknownMessage   = errors.New("UnknownMessageError")
	ErrInvalidMessage   = errors.New("InvalidMessageError")
	ErrInvalidLogIndex  = errors.New("InvalidLogIndexError")
	ErrInvalidNodeId    = errors.New("InvalidNodeIdError")
	ErrInvalidNodeAddr  = errors.New("InvalidNodeAddrError")
	ErrInvalidNodeIndex = errors.New("InvalidNodeIndexError")
	ErrInvalidTCPIP     = errors.New("InvalidTCPIPError")
)

const (
	MinElectionTime = 150
	MaxElectionTime = 300
)

type Log struct {
	Term    int
	Content []byte
}

type Status int

const (
	Follower Status = iota + 1
	Candidate
	Leader
)

//logもtermも0-indexedとしintにする

//logが空、len(logs)=0の時の取り扱いに注意
//AppendEntries
//prevIndex=-1,prevTerm=-1とする

//RequestVote
//lastIndex,lastTerm=-1とする

type Logs []Log

func (l Logs) checkBoundary(index int) bool {
	return 0 <= index && index <= len(l)-1
}

func (l *State) getLog(index int) (Log, bool) {
	if !l.logs.checkBoundary(index) {
		return Log{}, false
	}

	return l.logs[index], true
}

func (l *State) getLatest() (Log, bool) {
	if len(l.logs) == 0 {
		return Log{}, false
	}
	return l.logs[len(l.logs)-1], true
}

func (l *State) getLatestIndex() int {
	if len(l.logs) == 0 {
		return -1
	}

	return len(l.logs) - 1
}

func (l *State) getSendableLogs(nextIndex int) []Log {
	if len(l.logs) == 0 {
		return nil
	}

	if !l.logs.checkBoundary(nextIndex) {
		return nil
	}

	return l.logs[nextIndex:]
}

func (l *State) deleteLogs(index int) {
	if !l.logs.checkBoundary(index) {
		return
	}

	l.logs = l.logs[:index]
}

func (l *State) getLogTerm(index int) int {
	if len(l.logs) == 0 {
		return -1
	}

	if !l.logs.checkBoundary(index) {
		return -1
	}

	return l.logs[index].Term
}

func (r *Raft) deleteLogs(index int) {
	r.state.deleteLogs(index)
}

func (r *Raft) getLogTerm(index int) int {
	return r.state.getLogTerm(index)
}

//ロック掛けるのが難しすぎる...
//とりあえず、メッセージ処理の最初の部分と、各Status内のRaft変数変更をする関数にLockを掛けることにする
//とにかく同じRaftサーバー内で動くGoroutine同士で競合しなければいい

type State struct {
	//Persistent on all services
	currentTerm int
	votedFor    int //未投票の時は-1,termごとにリセット？
	logs        Logs

	//Volatile state on all servers
	commitIndex int //コミット済みログの最大index
	lastApplied int //fsmに適用されたログの最大index

	//Volatile state on Leader(選挙後に初期化)
	nextIndexes  map[Node]int //各サーバーに対し、次に送信するログのindex
	matchIndexes map[Node]int //各サーバーにて複製済みのログの最大index

}

//TODO TransportManagerとして分割
type Transport struct {
	net.Listener
}

func NewTransport(ln net.Listener) *Transport {
	return &Transport{
		ln,
	}
}

func (n *Transport) Dial(addr string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	return conn, err
}

func (n *Transport) Accept() (net.Conn, error) {
	conn, err := n.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (n *Transport) Close() error {
	return n.Listener.Close()

}

type Node struct {
	ServerId int
	Addr     string
}

type RaftMessage struct {
	addr    string
	payload Payload
}

func NewRaftMessage(addr string, payload Payload) *RaftMessage {
	return &RaftMessage{
		addr:    addr,
		payload: payload,
	}
}

//TODO 後々VoteManagerとして分割
type VotedInfo struct {
	m map[Node]bool
}

func NewVotedInfo(nodes []Node) *VotedInfo {

	m := make(map[Node]bool)

	for _, node := range nodes {
		m[node] = false
	}
	return &VotedInfo{
		m: m,
	}
}

func (v *VotedInfo) addVote(node Node) {
	v.m[node] = true
}

func (v *VotedInfo) reset() {
	for node, _ := range v.m {
		v.m[node] = false
	}
}

func (v *VotedInfo) getCurrentVote() int {
	voted := 0

	for _, isVoted := range v.m {
		if isVoted {
			voted++
		}
	}

	return int(voted)
}

var defaultCreateElectionTime = func() time.Duration {
	num := rand.Intn(MaxElectionTime-MinElectionTime) + MinElectionTime
	return time.Duration(num)
}

//pingInterval<=とすること
type Raft struct {
	requestCh  chan *RaftMessage
	streamCh   chan net.Conn
	shutdownCh chan struct{}
	stopCh     chan struct{}

	Transport      *Transport
	msgTimeout     time.Duration
	mu             sync.Mutex
	logger         *log.Logger
	messageManager *MessageManager

	nodes []Node //自分を含める

	pingInterval    time.Duration
	tickers         []*time.Ticker
	electionTimeout time.Duration

	state         *State
	currentStatus Status
	addr          string
	serverId      int

	votedInfo            *VotedInfo
	fsm                  FSM
	generateElectionTime func() time.Duration
}

func (r *Raft) isFollower() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentStatus == Follower
}

func (r *Raft) isLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentStatus == Leader
}

func (r *Raft) isCandidate() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentStatus == Candidate
}

//TODO retry処理を作る、今はshutdownb済みのやつにも送ってしまう
//retry回数失敗したらStatus=DeadにしてDeadのやつには送らないようにする？
func (r *Raft) send(addr string, payload []byte) {
	conn, err := r.Transport.Dial(addr, r.msgTimeout)
	if err != nil {
		r.logger.Println(err)
		return
	}
	defer conn.Close()

	_, err = conn.Write(payload)
	if err != nil {
		r.logger.Println(err)
		return
	}

}

func (r *Raft) sendToAllFollower(payload []byte) {
	for _, node := range r.nodes {

		if node.Addr == r.addr {
			continue
		}
		go r.send(node.Addr, payload)
	}
}

func (r *Raft) receive(conn net.Conn) ([]byte, error) {
	var b []byte
	_, err := conn.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil

}

type Config struct {
	serverAddr             string //include port
	msgTimeout             time.Duration
	pingInterval           time.Duration
	logger                 *log.Logger
	peerAddrs              []string
	fsm                    FSM
	generateElectionTimeFn func() time.Duration
}

//TODO のちのちNodeManagerとして分割

func makeNodes(c *Config) []Node {
	nodeLen := len(c.peerAddrs) + 1

	nodes := make([]Node, nodeLen)

	addrs := append([]string{c.serverAddr}, c.peerAddrs...)

	sort.Strings(addrs)

	for id, addr := range addrs {
		nodes[id] = Node{
			ServerId: id,
			Addr:     addr,
		}
	}

	return nodes
}

func initMaps(nodes []Node) (map[Node]int, map[Node]int) {
	nextIndexes := make(map[Node]int)
	matchIndexes := make(map[Node]int)

	for _, node := range nodes {
		nextIndexes[node] = 0
		matchIndexes[node] = -1
	}

	return nextIndexes, matchIndexes
}

func (r *Raft) getNodeByAddr(addr string) (Node, error) {
	for _, target := range r.nodes {
		if addr == target.Addr {
			return target, nil
		}
	}

	return Node{}, ErrInvalidNodeAddr
}

func (r *Raft) getNodeById(id int) (Node, error) {
	for _, target := range r.nodes {
		if id == target.ServerId {
			return target, nil
		}
	}

	return Node{}, ErrInvalidNodeId
}

func extractServerId(addr string, nodes []Node) int {
	for _, node := range nodes {

		if node.Addr == addr {
			return node.ServerId
		}
	}

	return -1
}

//MessageManagerだったりTransportだったりraftと直接関係ないところは後々interfaceにした方が良さそう
func NewRaft(c *Config) (*Raft, error) {
	rand.Seed(time.Now().UnixNano())

	ln, err := net.Listen("tcp", c.serverAddr)
	if err != nil {
		return nil, err
	}

	transport := NewTransport(ln)
	mm := NewMassageManager()

	//nextIndexesとmatchIndexesを初期化
	nodes := makeNodes(c)
	nextIndexes, matchIndexes := initMaps(nodes)
	voteInfo := NewVotedInfo(nodes)
	serverId := extractServerId(c.serverAddr, nodes)

	state := &State{
		nextIndexes:  nextIndexes,
		matchIndexes: matchIndexes,
	}

	if c.logger == nil {
		c.logger = log.Default()
	}

	if c.generateElectionTimeFn == nil {
		c.generateElectionTimeFn = defaultCreateElectionTime
	}

	if c.msgTimeout == 0 {
		c.msgTimeout = 10 * time.Second
	}
	if c.pingInterval == 0 {
		c.pingInterval = 100 * time.Millisecond
	}

	r := &Raft{
		Transport:      transport,
		msgTimeout:     c.msgTimeout,
		pingInterval:   c.pingInterval,
		logger:         c.logger,
		streamCh:       make(chan net.Conn),
		requestCh:      make(chan *RaftMessage),
		shutdownCh:     make(chan struct{}),
		stopCh:         make(chan struct{}),
		messageManager: mm,

		addr: c.serverAddr,

		state: state,

		fsm:                  c.fsm,
		votedInfo:            voteInfo,
		nodes:                nodes,
		generateElectionTime: c.generateElectionTimeFn,
		serverId:             serverId,
	}

	//最初はFollwerから起動する
	r.currentStatus = Follower
	r.state.votedFor = -1
	r.state.commitIndex = -1
	r.state.lastApplied = -1

	r.Run()

	return r, nil
}

func (r *Raft) GetServerName() string {
	return fmt.Sprintf("Server %d", r.serverId)
}

func (r *Raft) Run() {
	//メッセージ受信用
	go r.Serve()
	// //メッセージ処理用
	go r.MessageHandler()
	//stateに応じてrunLeader,runFollower,runCandidateを実行
	go r.runState()
}

func (r *Raft) runState() {
	for {

		select {
		case <-r.shutdownCh:
			return
		default:
		}

		switch r.currentStatus {
		case Follower:
			r.runFollower()
		case Candidate:
			r.runCandidate()
		case Leader:
			r.runLeader()
		}
	}
}

//TODO prevLogとprevLogTerm,の扱い,ここもやる
func (r *Raft) HeartBeat() {

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, node := range r.nodes {
		if node.Addr == r.addr {
			continue
		}

		nextIndex := r.state.nextIndexes[node]

		prevIndex, prevTerm := r.getPrevIndexAndTerm(nextIndex)

		payload := r.messageManager.CreateAppendEntriesRequest(
			r.state.currentTerm,
			prevIndex,
			prevTerm,
			r.state.commitIndex,
			nil,
			r.addr,
		)
		bytes, err := payload.Marshal()
		if err != nil {
			r.logger.Println(err)
			return
		}

		msg, err := r.messageManager.Create(APPEND_ENTRIES, r.addr, bytes)
		if err != nil {
			r.logger.Println(err)
			return
		}

		r.logger.Printf("%s send HeartBeat to %s:  prevIndex:%d,prevTerm:%d,nextIndex:%d,entriesLen:%d\n", r.GetServerName(), node.Addr, prevIndex, prevTerm, nextIndex, 0)

		go r.send(node.Addr, msg)
	}

	// r.sendToAllFollower(msg)

}

func (r *Raft) triggerFunc(duration time.Duration, C <-chan time.Time, stop <-chan struct{}, f func()) {
	for {
		select {
		case <-C:
			f()
		case <-stop:
			return
		}
	}
}

func (r *Raft) schedule() {

	t := time.NewTicker(r.pingInterval)
	go r.triggerFunc(r.pingInterval, t.C, r.stopCh, r.HeartBeat)
	r.tickers = append(r.tickers, t)

}

func (r *Raft) deschedule() {
	close(r.stopCh)
	for _, t := range r.tickers {
		t.Stop()
	}
	r.tickers = nil
}

func (r *Raft) applyFSM(index int) {
	log, exists := r.state.getLog(index)
	if !exists {
		return
	}
	r.fsm.Apply(log.Content)

	r.state.lastApplied = index

	//クライアントに返信
	//TODO 何を返信する?
}

func (r *Raft) ApplyFSM(start, end int) {
	for i := start; i <= end; i++ {
		r.applyFSM(i)
	}
}

//termが変わったらVoteForとかもリセット
func (r *Raft) goToNextTerm(term int) {
	r.state.currentTerm = term
	r.state.votedFor = -1
	r.votedInfo.reset()
}
func (r *Raft) commonProcessMessageOnAllStatus(term int) {

	if r.state.currentTerm < term {
		r.goToNextTerm(term)
		r.changeCurrentStatus(Follower)
		return
	}
}

func (r *Raft) calculateNextIndexForLeaderStart() int {
	if len(r.state.logs) == 0 {
		return 0
	}

	//logs[a,b,c,d]としてこの時dがindex=3で、nextIndexは4

	return int(len(r.state.logs))
}

func (r *Raft) getPrevIndexAndTerm(nextIndex int) (int, int) {
	if len(r.state.logs) == 0 {
		return -1, -1
	}

	prevIndex := nextIndex - 1
	prevLog, exists := r.state.getLog(prevIndex)
	if !exists {
		return -1, -1
	}

	return prevIndex, prevLog.Term

}

func (r *Raft) resetStateOnLeader() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for k, _ := range r.state.nextIndexes {
		r.state.nextIndexes[k] = r.calculateNextIndexForLeaderStart()
	}

	for k, _ := range r.state.matchIndexes {
		r.state.matchIndexes[k] = -1
	}
}

func (r *Raft) handleRaftAppendEntriesResponse(addr string, payload *AppendEntriesResponse) {

	r.logger.Printf("%s get AppendEntryResponse from %s\n", r.GetServerName(), addr)

	node, err := r.getNodeByAddr(addr)
	if err != nil {
		r.logger.Println(err)
		return
	}

	// N > commitIndex、過半数の matchIndex[i] ≧ N、log[N].term == currentTerm となる N が存在する場合: commitIndex = N に設定 (§5.3, §5.4)。
	//成功したときはcommitIndexが関わってくる

	if payload.Success {
		//論文のFigure7.の例でリーダーと(b)を使って考えると、
		//nextIndex=4で成功するはず
		//そのときはindex5~10をentriesとして送っている
		//なので次に送る予定の11(まだリーダーのログにはないかも)をnextIndex
		//matchIndexは対象のfollowerに複製済みの最大indexなので、今回複製した最大の10が入る...でいいはず
		r.state.nextIndexes[node] = len(r.state.logs)

		if len(r.state.logs) == 0 {
			r.state.matchIndexes[node] = -1
		} else {
			r.state.matchIndexes[node] = len(r.state.logs) - 1
		}
		return
	}

	//失敗したときはdecrement and ReSendAppendEntries
	_, exists := r.state.nextIndexes[node]
	if !exists {
		r.logger.Println(ErrInvalidNodeIndex)
		return
	}

	r.state.nextIndexes[node]--
	nextIndex := r.state.nextIndexes[node]
	prevIndex, prevTerm := r.getPrevIndexAndTerm(nextIndex)

	entries := r.state.getSendableLogs(nextIndex)

	aePayload := r.messageManager.CreateAppendEntriesRequest(
		r.state.currentTerm,
		prevIndex,
		prevTerm,
		r.state.commitIndex,
		entries,
		r.addr,
	)
	bytes, err := aePayload.Marshal()
	if err != nil {
		r.logger.Println(err)
		return
	}

	msg, err := r.messageManager.Create(APPEND_ENTRIES, r.addr, bytes)
	if err != nil {
		r.logger.Println(err)
		return
	}

	r.send(addr, msg)

}

func (r *Raft) handleLeaderClientMessage(payload *ClientMessage) {

	r.logger.Printf("%s add log to its Local Logs...\n", r.GetServerName())
	//ローカルログにエントリを追加
	r.state.logs = append(r.state.logs, Log{
		Term:    r.state.currentTerm,
		Content: payload.Content,
	})

	for _, node := range r.nodes {

		if node.Addr == r.addr {
			continue
		}

		latestLeaderIndex := r.state.getLatestIndex()
		nodeNextIndex := r.state.nextIndexes[node]
		if nodeNextIndex <= latestLeaderIndex {
			prevIndex, prevTerm := r.getPrevIndexAndTerm(nodeNextIndex)

			entries := r.state.getSendableLogs(nodeNextIndex)

			aePayload := r.messageManager.CreateAppendEntriesRequest(
				r.state.currentTerm,
				prevIndex,
				prevTerm,
				r.state.commitIndex,
				entries,
				r.addr,
			)
			bytes, err := aePayload.Marshal()
			if err != nil {
				r.logger.Println(err)
				return
			}

			msg, err := r.messageManager.Create(APPEND_ENTRIES, r.addr, bytes)
			if err != nil {
				r.logger.Println(err)
				return
			}

			r.logger.Printf("%s send AppendEntries to %s:  prevIndex:%d,prevTerm:%d,nextIndex:%d,entriesLen:%d\n", r.GetServerName(), node.Addr, prevIndex, prevTerm, nodeNextIndex, len(entries))
			go r.send(node.Addr, msg)
		}
	}
}

func (r *Raft) handleLeaderMessage(message *RaftMessage) {

	r.mu.Lock()
	defer r.mu.Unlock()

	switch payload := message.payload.(type) {
	case *AppendEntriesRequest:
		r.logger.Println(ErrInvalidMessage)
		return
	case *RequestVoteRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVote(message.addr, payload)
		return
	case *AppendEntriesResponse:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftAppendEntriesResponse(message.addr, payload)
		return
	case *RequestVoteResponse:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVoteResponse(message.addr, payload)
		return
	case *ClientMessage:
		r.handleLeaderClientMessage(payload)
		return
	default:
		r.logger.Println(ErrUnknownMessage)
		return
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (r *Raft) RequireQuorum() int {
	return int(math.Ceil(float64(len(r.nodes)) / float64(2)))
}

func (r *Raft) checkIndexMatch(targetIndex int) bool {
	var quorum int
	for _, index := range r.state.matchIndexes {
		if targetIndex <= index {
			quorum++
		}
	}

	return quorum >= r.RequireQuorum()
}

func (r *Raft) checkLogTerm(targetIndex int) bool {
	targetLog, exists := r.state.getLog(targetIndex)
	if !exists {
		return false
	}

	return targetLog.Term == r.state.currentTerm
}

func (r *Raft) satisfyQuorum(index int) bool {
	//targetLogIndex <= matchIndex on SpecificNode の関係なら、あるNodeにおいてtargetLogIndexは複製済み
	//checkLoｇTermは論文5.4節のエッジケースを防ぐ条件
	if r.checkIndexMatch(index) && r.checkLogTerm(index) {
		return true
	}
	return false
}

func (r *Raft) checkCommit() {

	r.mu.Lock()
	defer r.mu.Unlock()

	//現在のleaderのcommit済みindexであるr.state.commitIndexよりindexが上のlogsをコミットしても良いか確認
	commitIndex := r.state.commitIndex
	newCommitIndex := commitIndex

	if len(r.state.logs)-1 <= commitIndex {
		return
	}

	for i := commitIndex + 1; i <= len(r.state.logs)-1; i++ {
		//定足数を超えているかチェック
		if r.satisfyQuorum(i) {
			newCommitIndex = max(newCommitIndex, i)
		}
	}

	//commitIndexを更新
	r.state.commitIndex = newCommitIndex

}

//checkLastApplied
//commitIndex > lastApplied の場合: lastApplied をインクリメントし、log[lastApplied] をステートマシンに適用
//commitとfsmAplliedのタイミングずらすべき？
//ずらすならcommitのみで落ちたときも復旧後何も意識せず回復可能
//ずらさずcommitとfsmAppliedを一緒にするなら新しくcommitされないとappliedされないので、サーバー再起動時にfsmAppliedをしないといけなさそう
func (r *Raft) checkLastApplied() {

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state.commitIndex > r.state.lastApplied {
		r.ApplyFSM(r.state.lastApplied+1, r.state.commitIndex)
	}
}

// func (r *Raft) checkAppendEntries() {

// 	r.mu.Lock()
// 	defer r.mu.Unlock()

// 	for _, node := range r.nodes {

// 		if node.Addr == r.addr {
// 			continue
// 		}

// 		latestLeaderIndex := r.state.getLatestIndex()
// 		nodeNextIndex := r.state.nextIndexes[node]
// 		if nodeNextIndex <= latestLeaderIndex {
// 			prevIndex, prevTerm := r.getPrevIndexAndTerm(nodeNextIndex)

// 			entries := r.state.getSendableLogs(nodeNextIndex)

// 			aePayload := r.messageManager.CreateAppendEntriesRequest(
// 				r.state.currentTerm,
// 				prevIndex,
// 				prevTerm,
// 				r.state.commitIndex,
// 				entries,
// 				r.addr,
// 			)
// 			bytes, err := aePayload.Marshal()
// 			if err != nil {
// 				r.logger.Println(err)
// 				return
// 			}

// 			msg, err := r.messageManager.Create(APPEND_ENTRIES, r.addr, bytes)
// 			if err != nil {
// 				r.logger.Println(err)
// 				return
// 			}

// 			r.logger.Printf("%s send AppendEntries to %s:  prevIndex:%d,prevTerm:%d,nextIndex:%d,entriesLen:%d\n", r.GetServerName(), node.Addr, prevIndex, prevTerm, nodeNextIndex, len(entries))
// 			go r.send(node.Addr, msg)
// 		}
// 	}
// }

func (r *Raft) runLeader() {

	r.logger.Printf("%s Go To Leader\n", r.addr)

	r.resetStateOnLeader()

	go r.schedule()

	defer func() {
		r.deschedule()
	}()

	for {

		select {
		case <-r.shutdownCh:
			return
		default:
		}

		//下の三つはデーモンとしてforの外で動かした方が良さそう
		r.checkLastApplied()

		r.checkCommit()

		// r.checkAppendEntries()

		select {
		case message := <-r.requestCh:
			r.handleLeaderMessage(message)
			if r.isFollower() {
				return
			}
		default:
		}

	}
}

//followerの場合clientからmessageがきたらリーダーに流す、candidateは？
func (r *Raft) handleClientMessageToLeader(payload *ClientMessage) {

}

func timerReset(t *time.Timer, resetTime time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
			//時間切れの場合t.Cを読み捨て
		default:
		}
	}

	t.Reset(resetTime)
}

func (r *Raft) checkAECondition(addr string, payload *AppendEntriesRequest) bool {
	checkLog := func() bool {
		if payload.PrevLogIndex == -1 && payload.PrevLogTerm == -1 {
			return true
		}
		//ここで存在しないことを確認しているのでPrevLogIndexに-1がきてもOKだけど事前に-1のチェックをしてもいい
		log, exists := r.state.getLog(payload.PrevLogIndex)

		if !exists {
			return false
		}

		return log.Term == payload.PrevLogTerm
	}

	isLogConflict := func(followerIndex, leaderIndex, followerTerm, leaderTerm int) bool {
		return followerIndex == leaderIndex && followerTerm != leaderTerm
	}

	min := func(a, b int) int {
		if a > b {
			return b
		}

		return a
	}

	// success := true

	//1.
	if payload.Term < r.state.currentTerm {
		return false
	}

	//2.
	if !checkLog() {
		return false
	}

	//3.indexがリーダーとフォロワーで同じで、termが違う場合、以降をすべて削除
	for index, log := range r.state.logs {
		followerInd := index
		followerTerm := log.Term
		if isLogConflict(followerInd, payload.PrevLogIndex, followerTerm, payload.PrevLogTerm) {
			r.deleteLogs(followerInd)
			//以降をすべて削除するので、これ以降をforで回す必要はない
			break
		}
	}

	//TODO ここのealryReturn部分ちょっと違いそうなので多分修正
	//ここまででsuccess=falseだったらealryReturn?
	// if !success {
	// 	return false
	// }

	//successがtrueの時のみ指定されたindex以降を複製、つまりpayload.entries
	//4.まだエントリにないログだったら追加

	if len(payload.Entries) != 0 {
		r.state.logs = append(r.state.logs, payload.Entries...)
	}

	//5.
	if payload.LeaderCommit > r.state.commitIndex {
		r.state.commitIndex = min(payload.LeaderCommit, len(r.state.logs))
	}

	return true
}

func (r *Raft) handleRaftAppendEntries(addr string, payload *AppendEntriesRequest) {

	success := r.checkAECondition(addr, payload)

	resPayload := r.messageManager.CreateAppendEntriesResponse(
		r.state.currentTerm,
		success,
	)
	bytes, err := resPayload.Marshal()
	if err != nil {
		r.logger.Println(err)
		return
	}

	msg, err := r.messageManager.Create(APPEND_ENTRIES_RESPONSE, r.addr, bytes)
	if err != nil {
		r.logger.Println(err)
		return
	}

	r.logger.Printf("%s send AppendEntriesResponse to %s\n", r.GetServerName(), addr)

	r.send(addr, msg)

}

//RPCを処理する前に必ず
//Rules for ServersのAllServersの二番目
// RPC リクエストまたはレスポンスがターム T > currentTerm を含む場合: currentTerm = T に設定し、フォロワーに転向
//をすること
//理由として、term5でvotedFor=ServerBの状態とする
//RequestVoteRPC{term:6,candidate:B}が来た時に最初にtermを変えておかないと投票できなくなるため
func (r *Raft) handleFollowerMessage(message *RaftMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch payload := message.payload.(type) {
	case *AppendEntriesRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftAppendEntries(message.addr, payload)
		return
	case *RequestVoteRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVote(message.addr, payload)
		return
	case *AppendEntriesResponse:
		r.logger.Println(ErrInvalidMessage)
		return
	case *RequestVoteResponse:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVoteResponse(message.addr, payload)
		return
	default:
		r.logger.Println(ErrUnknownMessage)
		return
	}

}

//requestとrequestResじゃなくてmessageChに統一した方がいいかも？
//そもそも各遷移状態でrequestChとか使いまわせるかを考えないとダメ

//使いまわすんだったら新しい状態に入るときにchを新しく作り直して、状態が終わるときにchにnilを入れるとか？
func (r *Raft) runFollower() {

	r.logger.Printf("%s Go To Follower\n", r.GetServerName())

	// t := time.NewTimer(r.electionTimeout)
	electionTime := r.generateElectionTime()
	t := time.NewTimer(electionTime)
	for {

		select {
		case <-r.shutdownCh:
			return
		default:
		}

		r.checkLastApplied()

		select {
		case message := <-r.requestCh:
			r.handleFollowerMessage(message)
			//timerReset
			timerReset(t, electionTime)
		case <-t.C:
			//状態とかいじる系はlock掛けた方がいい？あとで検討
			//選挙開始、term+1する
			r.startNewElection()
			return
		}
	}
}

func (r *Raft) sendRequestVote() {
	meNode, err := r.getNodeByAddr(r.addr)
	if err != nil {
		r.logger.Println(err)
		return
	}

	lastLogIndex := r.state.getLatestIndex()

	lastLogTerm := r.state.getLogTerm(int(lastLogIndex))

	payload := r.messageManager.CreateRequestVoteRequest(
		r.state.currentTerm,
		int(meNode.ServerId),
		int(lastLogIndex),
		lastLogTerm,
	)
	bytes, err := payload.Marshal()
	if err != nil {
		r.logger.Println(err)
		return
	}

	msg, err := r.messageManager.Create(REQUEST_VOTE, r.addr, bytes)
	if err != nil {
		r.logger.Println(err)
		return
	}

	r.logger.Printf("%s send RequestVote\n", r.GetServerName())

	r.sendToAllFollower(msg)
}

//Candidateの場合LeaderにResponseって返すのか？
//Requestが来たら多分返すで良い
// func (r *Raft) handleCandidateAppendEntries(message *AppendEntriesRequest) bool {

// 	//☆ 新しいリーダーの時は自分がFollowerになる
// 	if message.Term < r.state.currentTerm {
// 		return false
// 	}

// 	return true
// }

func (r *Raft) checkVoteGranted(payload *RequestVoteRequest) bool {
	//1タームにつき一人の候補者にしか投票できない
	if payload.Term < r.state.currentTerm {
		return false
	}

	canVote := func() bool {
		//これってもし同じCandidateから二回同じRequestVoteが来たら二回投票にならない？選挙の制限のとこで説明ある？
		return r.state.votedFor == -1 || r.state.votedFor == payload.CandidateId
	}

	//logが最新かを判断しないといけない
	//最新かどうかの判断基準は下記
	// 	Raft determines which of two logs is more up-to-date
	// by comparing the index and term of the last entries in the
	// logs. If the logs have last entries with different terms, then
	// the log with the later term is more up-to-date. If the logs
	// end with the same term, then whichever log is longer is
	// more up-to-date.
	isLogLatest := func() bool {
		//自分の側にログがなくて相手の側にログあるときは相手の方が最新でいいはず
		latestLog, logExists := r.state.getLatest()
		if !logExists {
			return true
		}
		myLatestIndex, myLatestTerm := len(r.state.logs), latestLog.Term

		if myLatestTerm != payload.LastLogterm {
			return myLatestTerm < payload.LastLogterm
		}

		return myLatestIndex <= int(payload.LastLogIndex)
	}

	if canVote() && isLogLatest() {
		//ここで候補者に投票？
		r.state.votedFor = payload.CandidateId
		return true
	}

	return false
}

func (r *Raft) handleRaftRequestVote(addr string, payload *RequestVoteRequest) {

	res := r.messageManager.CreateRequestVoteResponse(r.state.currentTerm, r.checkVoteGranted(payload))

	bytes, err := res.Marshal()
	if err != nil {
		r.logger.Panicln(err)
		return
	}

	msg, err := r.messageManager.Create(REQUEST_VOTE_RESPONSE, r.addr, bytes)
	if err != nil {
		r.logger.Println(err)
		return
	}

	//候補者へ返信
	r.logger.Printf("%s get RequestVoteRequest: from: %s\n", r.GetServerName(), addr)
	r.send(addr, msg)
}

//☆　過半数獲得でリーダー
func (r *Raft) shouldLeader() bool {

	r.mu.Lock()
	defer r.mu.Unlock()

	requiredVote := int(math.Ceil(float64(len(r.nodes)) / float64(2)))
	return r.votedInfo.getCurrentVote() >= requiredVote
}

func (r *Raft) createElectionTimer() *time.Timer {

	return time.NewTimer(r.generateElectionTime() * time.Millisecond)
}

func (r *Raft) changeCurrentStatus(status Status) {
	r.currentStatus = status
}

func (r *Raft) startNewElection() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.logger.Printf("%s start NewElection\n", r.GetServerName())
	r.goToNextTerm(r.state.currentTerm + 1)
	r.changeCurrentStatus(Candidate)
}

func (r *Raft) handleRaftRequestVoteResponse(addr string, payload *RequestVoteResponse) {
	r.logger.Printf("%s get RequestVoteResponse: from: %s\n", r.GetServerName(), addr)
	node, err := r.getNodeByAddr(addr)
	if err != nil {
		r.logger.Println(err)
		return
	}
	if payload.VoteGranted {
		r.votedInfo.addVote(node)
	}
}

func (r *Raft) handleCandidateMessage(message *RaftMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch payload := message.payload.(type) {
	case *AppendEntriesRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftAppendEntries(message.addr, payload)
		return
	case *RequestVoteRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVote(message.addr, payload)
		return
	case *AppendEntriesResponse:
		r.logger.Println(ErrInvalidMessage)
		return
	case *RequestVoteResponse:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleRaftRequestVoteResponse(message.addr, payload)
		return
	default:
		r.logger.Println(ErrUnknownMessage)
		return
	}

}

func (r *Raft) voteToMe() {
	r.mu.Lock()
	defer r.mu.Unlock()

	meNode, err := r.getNodeByAddr(r.addr)
	if err != nil {
		r.logger.Println(err)
		return
	}
	r.state.votedFor = meNode.ServerId
	r.votedInfo.addVote(meNode)
}

func (r *Raft) runCandidate() {

	r.logger.Printf("%s Go To Candidate\n", r.GetServerName())

	t := r.createElectionTimer()
	//自分自身に投票
	r.voteToMe()

	//RequestVoteRPCを他のサーバーに発行
	r.mu.Lock()
	r.sendRequestVote()
	r.mu.Unlock()

	for {
		select {
		case <-r.shutdownCh:
			return
		default:
		}

		r.checkLastApplied()

		if r.shouldLeader() {
			r.mu.Lock()
			r.changeCurrentStatus(Leader)
			r.mu.Unlock()
			return
		}

		select {
		case message := <-r.requestCh:
			r.handleCandidateMessage(message)
			if r.isFollower() {
				return
			}
		case <-t.C:
			r.startNewElection()
			return

		default:

		}
	}
}

//raft本来の処理に関係ない奴はconnectionManegerに移した方が良さそう
//Shutdownしてshutdownをclose,listernerもCloseすると、Acceptでuse of closed network connectionが出てしまう
//回避する方法ある？
//Acceptでuse of closed network connectionでてかつshutDown済みだったらエラー吐かないようにすればいいはず
//shutDownChだけforの先頭に置く使い方ならchannelじゃなくatomicBooleanでいい気もする
//他のselectの中に混ぜるならchannelのままでいいけど
func (r *Raft) Serve() {
	for {

		select {
		case <-r.shutdownCh:
			return
		default:
		}

		conn, err := r.Transport.Accept()
		if err != nil {
			select {
			case <-r.shutdownCh:
				return
			default:
				log.Fatal(err)
				return
			}
		}
		r.streamCh <- conn
	}
}

func (r *Raft) StreamCh() <-chan net.Conn {
	return r.streamCh
}

func (r *Raft) MessageHandler() {
	for {
		select {
		case conn := <-r.StreamCh():
			go r.handleMessage(conn)
		case <-r.shutdownCh:
			return
		}
	}
}

func (r *Raft) handleMessage(conn net.Conn) {
	defer conn.Close()

	bufConn := bufio.NewReader(conn)

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, bufConn); err != nil {
		r.logger.Println(err)
		return
	}

	msg, err := r.messageManager.Parse(buf.Bytes())
	if err != nil {
		r.logger.Println(err)
		return
	}

	switch msg.MessageType {
	case APPEND_ENTRIES:
		r.handleAppendEntries(msg.Payload, msg.Addr)
		return
	case REQUEST_VOTE:
		r.handleRequestVote(msg.Payload, msg.Addr)
		return
	case APPEND_ENTRIES_RESPONSE:
		r.handleAppendEntriesResponse(msg.Payload, msg.Addr)
		return
	case REQUEST_VOTE_RESPONSE:
		r.handleRequestVoteResponse(msg.Payload, msg.Addr)
		return
	case CLIENT_MESSAGE:
		r.handleClientMessage(msg.Payload, msg.Addr)
		return
	default:
		r.logger.Println(ErrUnknownMessage)
		return
	}

}

func (r *Raft) handleClientMessage(payload []byte, addr string) {
	message := &ClientMessage{}
	if err := message.Unmarshal(payload); err != nil {
		r.logger.Println(err)
		return
	}

	r.requestCh <- NewRaftMessage(addr, message)

}

func (r *Raft) handleAppendEntries(payload []byte, addr string) {
	message := &AppendEntriesRequest{}
	if err := message.Unmarshal(payload); err != nil {
		r.logger.Println(err)
		return
	}

	r.requestCh <- NewRaftMessage(addr, message)

}

func (r *Raft) handleRequestVote(payload []byte, addr string) {
	message := &RequestVoteRequest{}
	if err := message.Unmarshal(payload); err != nil {
		r.logger.Println(err)
		return
	}

	r.requestCh <- NewRaftMessage(addr, message)
}

func (r *Raft) handleAppendEntriesResponse(payload []byte, addr string) {
	message := &AppendEntriesResponse{}
	if err := message.Unmarshal(payload); err != nil {
		r.logger.Println(err)
		return
	}

	r.requestCh <- NewRaftMessage(addr, message)

}

func (r *Raft) handleRequestVoteResponse(payload []byte, addr string) {
	message := &RequestVoteResponse{}
	if err := message.Unmarshal(payload); err != nil {
		r.logger.Println(err)
		return
	}

	r.requestCh <- NewRaftMessage(addr, message)
}

func (r *Raft) ShutDown() {

	select {
	case <-r.shutdownCh:
		return
	default:
	}

	close(r.shutdownCh)
	err := r.Transport.Close()
	if err != nil {
		r.logger.Println(err)
	}

	r.logger.Printf("%s is Shutdown...\n", r.addr)
}
