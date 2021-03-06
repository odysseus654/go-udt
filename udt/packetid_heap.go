package udt

import "github.com/odysseus654/go-udt/udt/packet"

// packetIdHeap defines a list of sorted packet IDs
type packetIDHeap []packet.PacketID

func (h packetIDHeap) Len() int {
	return len(h)
}

func (h packetIDHeap) Less(i, j int) bool {
	return h[i].Seq < h[j].Seq
}

func (h packetIDHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *packetIDHeap) Push(x interface{}) { // Push and Pop use pointer receivers because they modify the slice's length, not just its contents.
	*h = append(*h, x.(packet.PacketID))
}

func (h *packetIDHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// Min does a binary search of the heap for the entry with the lowest packetID greater than or equal to the specified value
func (h packetIDHeap) Min(greaterEqual packet.PacketID, lessEqual packet.PacketID) (packet.PacketID, int) {
	len := len(h)
	idx := 0
	wrapped := greaterEqual.Seq > lessEqual.Seq
	for idx < len {
		pid := h[idx]
		var next int
		if pid.Seq == greaterEqual.Seq {
			return h[idx], idx
		} else if pid.Seq >= greaterEqual.Seq {
			next = idx * 2
		} else {
			next = idx*2 + 1
		}
		if next >= len && h[idx].Seq > greaterEqual.Seq && (wrapped || h[idx].Seq <= lessEqual.Seq) {
			return h[idx], idx
		}
		idx = next
	}

	// can't find any packets with greater value, wrap around
	if wrapped {
		idx = 0
		for {
			next := idx * 2
			if next >= len && h[idx].Seq <= lessEqual.Seq {
				return h[idx], idx
			}
			idx = next
		}
	}
	return packet.PacketID{Seq: 0}, -1
}

func (h packetIDHeap) compare(pktID packet.PacketID, idx int) int {
	if pktID.Seq < h[idx].Seq {
		return -1
	}
	if pktID.Seq > h[idx].Seq {
		return +1
	}
	return 0
}

// Find does a binary search of the heap for the specified packetID which is returned
func (h packetIDHeap) Find(pktID packet.PacketID) (*packet.PacketID, int) {
	len := len(h)
	idx := 0
	for idx < len {
		cmp := h.compare(pktID, idx)
		if cmp == 0 {
			return &h[idx], idx
		} else if cmp > 0 {
			idx = idx * 2
		} else {
			idx = idx*2 + 1
		}
	}
	return nil, -1
}
