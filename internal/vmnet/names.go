// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"container/list"
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

// The synthetic address space DNS answers come from: 198.18.0.0/15 (RFC 2544
// benchmarking space, never routed). Offsets 0 and 2^17-1 (the /15's network
// and broadcast addresses) are never handed out.
const (
	synthBase  = 198<<24 | 18<<16
	synthHosts = 1<<17 - 2
	answerTTL  = 60 * time.Second
	maxNames   = 4096
)

// validName reports whether name (lowercased, no trailing dot) is a hostname
// the relay will put on a CONNECT line: LDH labels of 1-63 bytes, no leading or
// trailing hyphen, 253 bytes in all. Anything else (CR, LF, space, ':', a
// control byte, an empty label) is refused before it reaches the proxy.
func validName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// nameTable maps the names the guest resolved to synthetic addresses and back.
// An address is derived from the name (FNV-1a over the /15's usable hosts), so
// a fresh relay — a restored or forked VM's — answers the same name with the
// same address whenever no collision intervened; a collision takes the next
// free address. The table is capped: the least recently used entry is evicted
// only once its last use is older than the answer TTL, so an address the guest
// may still hold in its cache is never relabelled, and a table full of live
// names refuses the query instead.
type nameTable struct {
	mu     sync.Mutex
	now    func() time.Time
	cap    int
	byName map[string]*list.Element
	byAddr map[uint32]*list.Element
	lru    *list.List // front = most recently used
}

type nameEntry struct {
	name string
	addr uint32
	used time.Time
}

func newNameTable(now func() time.Time, capacity int) *nameTable {
	return &nameTable{
		now: now, cap: capacity,
		byName: map[string]*list.Element{}, byAddr: map[uint32]*list.Element{}, lru: list.New(),
	}
}

// resolve returns name's address, adding it when new. evicted is the name an
// addition displaced; ok=false means the table is full of live names.
func (t *nameTable) resolve(name string) (addr [4]byte, evicted string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if el, hit := t.byName[name]; hit {
		e := el.Value.(*nameEntry)
		e.used = now
		t.lru.MoveToFront(el)
		return toAddr(e.addr), "", true
	}
	if t.lru.Len() >= t.cap {
		back := t.lru.Back()
		old := back.Value.(*nameEntry)
		if now.Sub(old.used) < answerTTL {
			return [4]byte{}, "", false
		}
		t.lru.Remove(back)
		delete(t.byName, old.name)
		delete(t.byAddr, old.addr)
		evicted = old.name
	}
	off := derive(name)
	for {
		if _, taken := t.byAddr[synthBase+off]; !taken {
			break
		}
		off = off%synthHosts + 1
	}
	e := &nameEntry{name: name, addr: synthBase + off, used: now}
	el := t.lru.PushFront(e)
	t.byName[name], t.byAddr[e.addr] = el, el
	return toAddr(e.addr), evicted, true
}

// lookup names a synthetic address the guest connected to, counting as a use.
func (t *nameTable) lookup(addr [4]byte) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.byAddr[uint32(addr[0])<<24|uint32(addr[1])<<16|uint32(addr[2])<<8|uint32(addr[3])]
	if !ok {
		return "", false
	}
	e := el.Value.(*nameEntry)
	e.used = t.now()
	t.lru.MoveToFront(el)
	return e.name, true
}

// derive is name's home offset in 1..synthHosts.
func derive(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return h.Sum32()%synthHosts + 1
}

func toAddr(a uint32) [4]byte { return [4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)} }
