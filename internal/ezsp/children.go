package ezsp

import (
	"context"
	"fmt"
)

// Child-table commands.
const (
	FrameGetParentChildParameters FrameID = 0x0029
	FrameGetChildData             FrameID = 0x004A
)

// ChildCount reports how many children this node currently has in its child
// table. A parent with a full table advertises no end-device capacity in its
// beacon, and a joining device will not pick it.
//
//	childCount(1) parentEui64(8) parentNodeId(2)
func (c *Conn) ChildCount(ctx context.Context) (int, error) {
	params, err := c.command(ctx, FrameGetParentChildParameters, nil)
	if err != nil {
		return 0, err
	}
	r := newBuf(params)
	n := r.u8()
	return int(n), r.err
}

// Child is one occupied child-table entry.
type Child struct {
	Index  int
	IEEE   EUI64
	Type   NodeType
	NodeID uint16
}

// Child reads one child-table entry. ok is false for an empty slot.
//
//	status(1) eui64(8) type(1) id(2) ...
//
// Later fields (phy, power, timeout, green-power data) vary between EZSP
// versions and are not needed here.
func (c *Conn) Child(ctx context.Context, index int) (Child, bool, error) {
	params, err := c.command(ctx, FrameGetChildData, []byte{uint8(index)})
	if err != nil {
		return Child{}, false, err
	}
	r := newBuf(params)
	status := EmberStatus(r.u8())
	if r.err != nil {
		return Child{}, false, r.err
	}
	if !status.OK() {
		return Child{}, false, nil // EMBER_NOT_JOINED: nothing in this slot
	}
	ch := Child{Index: index, IEEE: r.ieee(), Type: NodeType(r.u8()), NodeID: r.u16()}
	if r.err != nil {
		return Child{}, false, fmt.Errorf("ezsp: getChildData %d: %w", index, r.err)
	}
	return ch, true, nil
}
