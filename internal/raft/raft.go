package raft

import (
	"bufio"
	"bytes"
	"errors"
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
	Term    uint64
	Content []byte
}

type Status int

const (
	Follower Status = iota + 1
	Candidate
	Leader
)

//logもtermも0-indexedとしuint64にする

//logが空、len(logs)=0の時の取り扱いに注意
//AppendEntries
//prevIndex=0,precvTerm=0とする

//RequestVote
//lastIndex,lastTerm=0とする

type Logs []Log

func (l Logs) checkBoundary(index uint64) bool {
	return index <= uint64(len(l)-1)
}

func (l *State) getLog(index uint64) (Log, bool) {
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

func (l *State) getLatestIndex() uint64 {
	if len(l.logs) == 0 {
		return 0
	}

	return uint64(len(l.logs) - 1)
}

func (l *State) getSendableLogs(nextIndex uint64) []Log {
	if len(l.logs) == 0 {
		return nil
	}

	if !l.logs.checkBoundary(nextIndex) {
		return nil
	}

	return l.logs[nextIndex:]
}

func (l *State) deleteLogs(index uint64) {
	if !l.logs.checkBoundary(index) {
		return
	}

	l.logs = l.logs[:index]
}

func (l *State) getLogTerm(index uint64) uint64 {
	if len(l.logs) == 0 {
		return 0
	}

	if !l.logs.checkBoundary(index) {
		return 0
	}

	return l.logs[index].Term
}

func (r *Raft) deleteLogs(index uint64) {
	r.state.deleteLogs(index)
}

func (r *Raft) getLogTerm(index uint64) uint64 {
	return r.state.getLogTerm(index)
}

type State struct {
	//Persistent on all services
	currentTerm uint64
	votedFor    int64 //未投票の時は-1,termごとにリセット？
	logs        Logs

	//Volatile state on all servers
	commitIndex uint64 //コミット済みログの最大index
	lastApplied uint64 //fsmに適用されたログの最大index

	//Volatile state on Leader(選挙後に初期化)
	nextIndexes  map[Node]uint64 //各サーバーに対し、次に送信するログのindex
	matchIndexes map[Node]uint64 //各サーバーにて複製済みのログの最大index

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
	ServerId int64
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
	m  map[Node]bool
	mu sync.Mutex
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
	v.mu.Lock()
	defer v.mu.Unlock()

	v.m[node] = true
}

func (v *VotedInfo) reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for node, _ := range v.m {
		v.m[node] = false
	}
}

func (v *VotedInfo) getCurrentVote() uint64 {
	voted := 0

	v.mu.Lock()
	defer v.mu.Unlock()

	for _, isVoted := range v.m {
		if isVoted {
			voted++
		}
	}

	return uint64(voted)
}

var defaultCreateElectionTime = func() time.Duration {
	num := rand.Intn(MaxElectionTime-MinElectionTime) + MinElectionTime
	return time.Duration(num)
}

//pingInterval<=とすること
type Raft struct {
	// appendCh     chan api.AppendEntriesRequest
	requestCh chan *RaftMessage
	// appendResCh  chan api.AppendEntriesResponse
	requestResCh chan *RaftMessage
	streamCh     chan net.Conn
	shutdownCh   chan struct{}
	stopCh       chan struct{}

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

	votedInfo            *VotedInfo
	fsm                  FSM
	generateElectionTime func() time.Duration
}

func (r *Raft) isFollower() bool {
	return r.currentStatus == Follower
}

func (r *Raft) isLeader() bool {
	return r.currentStatus == Leader
}

func (r *Raft) isCandidate() bool {
	return r.currentStatus == Candidate
}

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
		r.send(node.Addr, payload)
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
			ServerId: int64(id),
			Addr:     addr,
		}
	}

	return nodes
}

func initMaps(nodes []Node) (map[Node]uint64, map[Node]uint64) {
	nextIndexes := make(map[Node]uint64)
	matchIndexes := make(map[Node]uint64)

	for _, node := range nodes {
		nextIndexes[node] = 0
		matchIndexes[node] = 0
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

func (r *Raft) getNodeById(id int64) (Node, error) {
	for _, target := range r.nodes {
		if id == target.ServerId {
			return target, nil
		}
	}

	return Node{}, ErrInvalidNodeId
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
		messageManager: mm,

		addr: c.serverAddr,

		state: state,

		fsm:                  c.fsm,
		votedInfo:            voteInfo,
		nodes:                nodes,
		generateElectionTime: c.generateElectionTimeFn,
	}

	//最初はFollwerから起動する
	r.currentStatus = Follower
	r.state.votedFor = -1

	r.Run()

	return r, nil
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
	// the prevLogIndex/Term should refer to the log entry immediately preceding the first element of the entries[] field of the RPC arguments in the leader’s log.
	//とのことなので、HeartBeatのときprev~は0でいい？
	payload := r.messageManager.CreateAppendEntriesRequest(
		r.state.currentTerm,
		0,
		0,
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

	r.logger.Printf("%s send HeartBeat...\n", r.addr)

	r.sendToAllFollower(msg)

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

func (r *Raft) applyFSM(index uint64) {
	log, exists := r.state.getLog(index)
	if !exists {
		return
	}
	r.fsm.Apply(log.Content)

	r.state.lastApplied = index

	//クライアントに返信
	//TODO 何を返信する?
}

func (r *Raft) ApplyFSM(start, end uint64) {
	for i := start; i <= end; i++ {
		r.applyFSM(i)
	}
}

//termが変わったらVoteForとかもリセット
func (r *Raft) goToNextTerm(term uint64) {
	r.state.currentTerm = term
	r.state.votedFor = -1
	r.votedInfo.reset()
}
func (r *Raft) commonProcessMessageOnAllStatus(term uint64) {
	if r.state.currentTerm < term {
		r.goToNextTerm(term)
		r.changeCurrentStatus(Follower)
		return
	}
}

func (r *Raft) calculateNextIndex() uint64 {
	if len(r.state.logs) == 0 {
		return 0
	}

	//logs[a,b,c,d]としてこの時dがindex=3で、nextIndexは4

	return uint64(len(r.state.logs))
}

func (r *Raft) getPrevIndexAndTerm(nextIndex uint64) (uint64, uint64) {
	if len(r.state.logs) == 0 {
		return 0, 0
	}

	prevIndex := nextIndex - 1
	prevLog, exists := r.state.getLog(prevIndex)
	if !exists {
		return 0, 0
	}

	return prevIndex, prevLog.Term

}

// uint64(len(r.state.logs) + 1)なので、logが0個でもnextIndexは1となるので-1はこない
func (r *Raft) resetStateOnLeader() {
	for k, _ := range r.state.nextIndexes {
		r.state.nextIndexes[k] = r.calculateNextIndex()
	}

	for k, _ := range r.state.matchIndexes {
		r.state.matchIndexes[k] = 0
	}
}

func (r *Raft) handleLeaderAppendEntriesResponse(addr string, payload *AppendEntriesResponse) {
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
		r.state.nextIndexes[node] = uint64(len(r.state.logs))
		r.state.matchIndexes[node] = uint64(len(r.state.logs) - 1)
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
	//ローカルログにエントリを追加
	r.state.logs = append(r.state.logs, Log{
		Term:    r.state.currentTerm,
		Content: payload.Content,
	})

	//followerに複製?それともハートビートうんぬんで出来てる？
}

func (r *Raft) handleLeaderMessage(message *RaftMessage) {
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
		r.handleLeaderAppendEntriesResponse(message.addr, payload)
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

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func (r *Raft) RequireQuorum() int {
	return int(math.Ceil(float64(len(r.nodes)) / float64(2)))
}

func (r *Raft) checkIndexMatch(targetIndex uint64) bool {
	var quorum int
	for _, index := range r.state.matchIndexes {
		if targetIndex <= index {
			quorum++
		}
	}

	return quorum >= r.RequireQuorum()
}

func (r *Raft) checkLogTerm(targetIndex uint64) bool {
	targetLog, exists := r.state.getLog(targetIndex)
	if !exists {
		return false
	}

	return targetLog.Term == r.state.currentTerm
}

func (r *Raft) satisfyQuorum(index uint64) bool {
	//targetLogIndex <= matchIndex on SpecificNode の関係なら、あるNodeにおいてtargetLogIndexは複製済み
	if r.checkIndexMatch(index) && r.checkLogTerm(index) {
		return true
	}
	return false
}

func (r *Raft) checkCommit() {

	//現在のleaderのcommit済みindexであるr.state.commitIndexよりindexが上のlogsをコミットしても良いか確認
	commitIndex := r.state.commitIndex
	newCommitIndex := commitIndex

	if len(r.state.logs) < int(commitIndex+1) {
		return
	}

	for i := commitIndex + 1; i <= uint64(len(r.state.logs)); i++ {
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
	if r.state.commitIndex > r.state.lastApplied {
		r.ApplyFSM(r.state.lastApplied+1, r.state.commitIndex)
	}
}

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

		r.checkLastApplied()

		r.checkCommit()

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

// func (r *Raft) handleHeartBeat(addr string, message *AppendEntriesRequest) {

// }

func (r *Raft) handleFollowerAppendEntries(addr string, payload *AppendEntriesRequest) {
	checkLog := func() bool {
		if payload.PrevLogIndex == 0 && payload.PrevLogTerm == 0 {
			return false
		}
		//ここで存在しないことを確認しているのでPrevLogIndexに-1がきてもOKだけど事前に-1のチェックをしてもいい
		log, exists := r.state.getLog(payload.PrevLogIndex)

		if !exists {
			return false
		}

		return log.Term == uint64(payload.PrevLogTerm)
	}

	isLogConflict := func(followerIndex, leaderIndex, followerTerm, leaderTerm uint64) bool {
		return followerIndex == leaderIndex && followerTerm != leaderTerm
	}

	min := func(a, b int) int {
		if a > b {
			return b
		}

		return a
	}

	success := true

	//1.
	if payload.Term < r.state.currentTerm {
		success = false
	}

	//2.
	if !checkLog() {
		success = false
	}

	//3.indexがリーダーとフォロワーで同じで、termが違う場合、以降をすべて削除
	for index, log := range r.state.logs {
		followerInd := index
		followerTerm := log.Term
		if isLogConflict(uint64(followerInd), payload.PrevLogIndex, followerTerm, payload.PrevLogTerm) {
			r.deleteLogs(uint64(followerInd))
			//以降をすべて削除するので、これ以降をforで回す必要はない
			break
		}
	}

	//ここまででsuccess=falseだったらealryReturn?
	if !success {
		return
	}

	//successがtrueの時のみ指定されたindex以降を複製、つまりpayload.entries
	//4.まだエントリにないログだったら追加
	r.state.logs = append(r.state.logs, payload.Entries...)

	//5.
	if payload.LeaderCommit > r.state.commitIndex {
		r.state.commitIndex = uint64(min(int(payload.LeaderCommit), len(r.state.logs)))
	}
}

//RPCを処理する前に必ず
//Rules for ServersのAllServersの二番目
// RPC リクエストまたはレスポンスがターム T > currentTerm を含む場合: currentTerm = T に設定し、フォロワーに転向
//をすること
//理由として、term5でvotedFor=ServerBの状態とする
//RequestVoteRPC{term:6,candidate:B}が来た時に最初にtermを変えておかないと投票できなくなるため
func (r *Raft) handleFollowerMessage(message *RaftMessage) {

	switch payload := message.payload.(type) {
	case *AppendEntriesRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleFollowerAppendEntries(message.addr, payload)
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
	r.logger.Printf("%s Go To Follower\n", r.addr)

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

	lastLogTerm := r.state.getLogTerm(uint64(lastLogIndex))

	payload := r.messageManager.CreateRequestVoteRequest(
		r.state.currentTerm,
		uint64(meNode.ServerId),
		uint64(lastLogIndex),
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

	r.logger.Printf("%s send RequestVote\n", r.addr)

	r.sendToAllFollower(msg)
}

//Candidateの場合LeaderにResponseって返すのか？
//Requestが来たら多分返すで良い
func (r *Raft) handleCandidateAppendEntries(message *AppendEntriesRequest) bool {

	//☆ 新しいリーダーの時は自分がFollowerになる
	if message.Term < r.state.currentTerm {
		return false
	}

	return true
}

func (r *Raft) checkVoteGranted(payload *RequestVoteRequest) bool {
	//1タームにつき一人の候補者にしか投票できない
	if payload.Term < r.state.currentTerm {
		return false
	}

	canVote := func() bool {
		//これってもし同じCandidateから二回同じRequestVoteが来たら二回投票にならない？選挙の制限のとこで説明ある？
		return r.state.votedFor == -1 || r.state.votedFor == int64(payload.CandidateId)
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
		r.state.votedFor = int64(payload.CandidateId)
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
	r.logger.Printf("%s get RequestVoteRequest: from: %s\n", r.addr, addr)
	r.send(addr, msg)
}

//☆　過半数獲得でリーダー
func (r *Raft) shouldLeader() bool {

	requiredVote := uint64(math.Ceil(float64(len(r.nodes)) / float64(2)))
	return r.votedInfo.getCurrentVote() >= requiredVote
}

func (r *Raft) createElectionTimer() *time.Timer {

	return time.NewTimer(r.generateElectionTime() * time.Millisecond)
}

func (r *Raft) changeCurrentStatus(status Status) {
	r.currentStatus = status
}

func (r *Raft) startNewElection() {
	r.logger.Printf("%s start NewElection\n", r.addr)
	r.goToNextTerm(r.state.currentTerm + 1)
	r.changeCurrentStatus(Candidate)
}

func (r *Raft) handleRaftRequestVoteResponse(addr string, payload *RequestVoteResponse) {
	r.logger.Printf("%s get RequestVoteResponse: from: %s\n", r.addr, addr)
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

	switch payload := message.payload.(type) {
	case *AppendEntriesRequest:
		r.commonProcessMessageOnAllStatus(payload.Term)
		r.handleCandidateAppendEntries(payload)
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

func (r *Raft) runCandidate() {

	r.logger.Printf("%s Go To Candidate\n", r.addr)

	t := r.createElectionTimer()
	//自分自身に投票
	meNode, err := r.getNodeByAddr(r.addr)
	if err != nil {
		r.logger.Println(err)
		return
	}
	r.state.votedFor = meNode.ServerId
	r.votedInfo.addVote(meNode)

	//RequestVoteRPCを他のサーバーに発行
	r.sendRequestVote()

	for {
		select {
		case <-r.shutdownCh:
			return
		default:
		}

		r.checkLastApplied()

		if r.shouldLeader() {
			r.currentStatus = Leader
			return
		}

		select {
		case message := <-r.requestCh:
			r.handleCandidateMessage(message)
			if r.isFollower() {
				return
			}
		case <-t.C:
			//状態とかいじる系はlock掛けた方がいい？あとで検討
			//新しい選挙を開始、一旦runStateに戻る
			r.startNewElection()
			return

		default:

		}
	}
}

//raft本来の処理に関係ない奴はconnectionManegerに移した方が良さそう

func (r *Raft) Serve() {
	for {

		select {
		case <-r.shutdownCh:
			return
		default:
		}

		conn, err := r.Transport.Accept()
		if err != nil {
			log.Fatal(err)
			return
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
	default:
		r.logger.Println(ErrUnknownMessage)
		return
	}

}

//Requestの時は文末にRequestつけないでもいいかな...
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
	close(r.shutdownCh)
	err := r.Transport.Close()
	if err != nil {
		r.logger.Println(err)
	}
}
