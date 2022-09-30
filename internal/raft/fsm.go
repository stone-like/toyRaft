package raft

import (
	"encoding/json"
	"sync"
)

type FSM interface {
	Apply([]byte)
}

type KVStore struct {
	store map[string]string
	mu    sync.Mutex
}

type Content struct {
	Key   string
	Value string
}

func (c *Content) Marshal() ([]byte, error) {
	return json.Marshal(c)
}

func (c *Content) Unmarshal(content []byte) error {
	return json.Unmarshal(content, c)

}

func NewKVStore() *KVStore {
	return &KVStore{
		store: make(map[string]string),
	}
}

func (k *KVStore) Apply(payload []byte) error {
	c := &Content{}

	if err := c.Unmarshal(payload); err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	k.store[c.Key] = c.Value

	return nil
}
