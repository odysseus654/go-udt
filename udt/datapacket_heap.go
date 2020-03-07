package udt

import (
	"container/heap"

	"github.com/odysseus654/go-udt/udt/packet"
)

// receiveLossList defines a list of recvLossEntry records sorted by their packet ID
type dataPacketHeap []*packet.DataPacket

func (h dataPacketHeap) Len() int {
	return len(h)
}

func (h dataPacketHeap) Less(i, j int) bool {
	return h[i].Seq < h[j].Seq
}

func (h dataPacketHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *dataPacketHeap) Push(x interface{}) { // Push and Pop use pointer receivers because they modify the slice's length, not just its contents.
	*h = append(*h, x.(*packet.DataPacket))
}

func (h *dataPacketHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// Min returns the smallest packet ID in this heap
func (h dataPacketHeap) Min() (*packet.DataPacket, int) {
	len := len(h)
	idx := 0
	for {
		newIdx := idx * 2
		if newIdx >= len {
			return h[idx], idx
		}
		idx = newIdx
	}
	return nil, -1
}

// Find does a binary search of the heap for the specified packetID which is returned
func (h dataPacketHeap) Find(packetID uint32) (*packet.DataPacket, int) {
	len := len(h)
	idx := 0
	for idx < len {
		pid := h[idx].Seq
		if pid == packetID {
			return h[idx], idx
		} else if pid > packetID {
			idx = idx * 2
		} else {
			idx = idx*2 + 1
		}
	}
	return nil, -1
}

// Find does a binary search of the heap for the entry with the lowest packetID greater than the specified value
func (h dataPacketHeap) UpperBound(packetID uint32) (*packet.DataPacket, int) {
	len := len(h)
	idx := 0
	for idx < len {
		pid := h[idx].Seq
		if pid > packetID {
			idx = idx * 2
		} else {
			idx = idx*2 + 1
		}
	}
	return nil, -1
}

// Remove does a binary search of the heap for the specified packetID, which is removed
func (h *dataPacketHeap) Remove(packetID uint32) bool {
	len := len(*h)
	idx := 0
	for idx < len {
		pid := (*h)[idx].Seq
		if pid == packetID {
			heap.Remove(h, idx)
			return true
		} else if pid > packetID {
			idx = idx * 2
		} else {
			idx = idx*2 + 1
		}
	}
	return false
}
