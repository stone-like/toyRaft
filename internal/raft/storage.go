package raft

type Storage interface {
	Persist(currentTerm, votedFor int, Logs Logs)
	Restore() (int, int, Logs)
	ShouldRestore() bool
}

var _ Storage = (*FakeStorage)(nil)

type Info struct {
	CurrentTerm int
	VotedFor    int
	Logs        Logs
}

type FakeStorage struct {
	Info Info
}

func (s *FakeStorage) Persist(currentTerm, votedFor int, logs Logs) {
	s.Info.CurrentTerm = currentTerm
	s.Info.VotedFor = votedFor
	s.Info.Logs = logs
}

func (s *FakeStorage) Restore() (int, int, Logs) {
	return s.Info.CurrentTerm, s.Info.VotedFor, s.Info.Logs
}

func (s *FakeStorage) ShouldRestore() bool {
	return len(s.Info.Logs) != 0 || s.Info.VotedFor != -1 || s.Info.CurrentTerm != 0
}
