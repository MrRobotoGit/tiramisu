package torrent

import (
	"container/heap"
	"fmt"
	"time"
	"unsafe"
)

// Fields are ordered widest first so the struct packs: there's one of these per conn ranked.
type worseConnInput struct {
	LastHelpful        time.Time
	CompletedHandshake time.Time
	GetPeerPriority    func() (peerPriority, error)
	Pointer            uintptr

	peerPriorityErr  error
	peerPriority     peerPriority
	peerPriorityDone bool

	BadDirection bool
	Useful       bool
}

// getPeerPriority memoizes the peer priority lookup. Ranking runs single-threaded under the client
// lock, so this doesn't need to synchronize. Backported from anacrolix/torrent upstream (commit
// 746940840): the previous sync.Once field made every heap swap copy a lock.
func (me *worseConnInput) getPeerPriority() (peerPriority, error) {
	if !me.peerPriorityDone {
		me.peerPriority, me.peerPriorityErr = me.GetPeerPriority()
		me.peerPriorityDone = true
	}
	return me.peerPriority, me.peerPriorityErr
}

type worseConnLensOpts struct {
	incomingIsBad, outgoingIsBad bool
}

func worseConnInputFromPeer(p *PeerConn, opts worseConnLensOpts) worseConnInput {
	ret := worseConnInput{
		Useful:             p.useful(),
		LastHelpful:        p.lastHelpful(),
		CompletedHandshake: p.completedHandshake,
		Pointer:            uintptr(unsafe.Pointer(p)),
		GetPeerPriority:    p.peerPriority,
	}
	if opts.incomingIsBad && !p.outgoing {
		ret.BadDirection = true
	} else if opts.outgoingIsBad && p.outgoing {
		ret.BadDirection = true
	}
	return ret
}

// Less applies the connection ordering from lowest to highest desirability, deferring peer-priority
// lookup until earlier fields tie and falling back to the pointer for a total ordering. Backported
// from anacrolix/torrent upstream (commit 746940840): the previous multiless chain let the trailing
// pointer comparison overwrite the priority result, so priority never affected ranking.
func (l *worseConnInput) Less(r *worseConnInput) bool {
	if l.BadDirection != r.BadDirection {
		return l.BadDirection && !r.BadDirection
	}
	if l.Useful != r.Useful {
		return !l.Useful && r.Useful
	}
	if !l.LastHelpful.Equal(r.LastHelpful) {
		return l.LastHelpful.Before(r.LastHelpful)
	}
	if !l.CompletedHandshake.Equal(r.CompletedHandshake) {
		return l.CompletedHandshake.Before(r.CompletedHandshake)
	}
	lPeerPriority, lPeerPriorityErr := l.getPeerPriority()
	if lPeerPriorityErr == nil {
		rPeerPriority, rPeerPriorityErr := r.getPeerPriority()
		if rPeerPriorityErr == nil && lPeerPriority != rPeerPriority {
			return lPeerPriority < rPeerPriority
		}
	}
	if l.Pointer == r.Pointer {
		panic(fmt.Sprintf("cannot differentiate %#v and %#v", l, r))
	}
	return l.Pointer < r.Pointer
}

type worseConnSlice struct {
	conns []*PeerConn
	keys  []worseConnInput
}

func (me *worseConnSlice) initKeys(opts worseConnLensOpts) {
	me.keys = make([]worseConnInput, len(me.conns))
	for i, c := range me.conns {
		me.keys[i] = worseConnInputFromPeer(c, opts)
	}
}

var _ heap.Interface = &worseConnSlice{}

func (me worseConnSlice) Len() int {
	return len(me.conns)
}

func (me worseConnSlice) Less(i, j int) bool {
	return me.keys[i].Less(&me.keys[j])
}

func (me *worseConnSlice) Pop() interface{} {
	i := len(me.conns) - 1
	ret := me.conns[i]
	me.conns = me.conns[:i]
	return ret
}

func (me *worseConnSlice) Push(x interface{}) {
	panic("not implemented")
}

func (me worseConnSlice) Swap(i, j int) {
	me.conns[i], me.conns[j] = me.conns[j], me.conns[i]
	me.keys[i], me.keys[j] = me.keys[j], me.keys[i]
}
